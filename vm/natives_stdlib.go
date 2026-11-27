package vm

import (
	"fmt"
	"math"
	"strings"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/luastdlib"
)

func (s *State) checkStringArg(args []Value, idx int, fnName string) (string, error) {
	if idx >= len(args) {
		return "", fmt.Errorf("bad argument #%d to '%s' (string expected, got no value)", idx+1, fnName)
	}
	v := args[idx]
	if v.IsString() {
		return s.asString(v).Str(), nil
	}
	if v.IsNumber() {
		return s.FormatValue(v), nil
	}
	return "", fmt.Errorf("bad argument #%d to '%s' (string expected, got %s)", idx+1, fnName, v.TypeName())
}

func captureToValue(s *State, c luastdlib.CaptureVal, srcLen int) (Value, error) {
	if c.IsPos {
		return IntValue(c.Pos), nil
	}
	str := c.Str
	if len(str) > maxShortStringLen && len(str) < srcLen {
		var err error
		str, err = s.cloneStringWithCancel(str)
		if err != nil {
			return NilValue(), err
		}
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
		th := s.cur()
		savedTop := th.top
		baseTop := savedTop
		frames := th.framesSlice(s.arena)
		for fi := range th.nframe {
			cf := &frames[fi]
			if cl := s.frameClosure(cf); cl != nil {
				if end := cf.base + int(s.boundProtoOf(cl).maxStack); end > baseTop {
					baseTop = end
				}
			}
		}
		if err := s.ensureStackWithRoots(th, baseTop+total+2, NilValue(), prefix); err != nil {
			return nil, err
		}
		written := 0
		defer func() {
			if written > 0 {
				fillNil(th.stackSlice(s.arena)[baseTop : baseTop+written])
			}
			th.top = savedTop
		}()
		for _, pv := range prefix {
			th.stackSlice(s.arena)[baseTop+written] = pv
			written++
		}
		th.top = baseTop + written
		for _, c := range caps {
			cv, cerr := captureToValue(s, c, srcLen)
			if cerr != nil {
				return nil, cerr
			}
			th.stackSlice(s.arena)[baseTop+written] = cv
			written++
			th.top = baseTop + written
		}
		out := make([]Value, written)
		copy(out, th.stackSlice(s.arena)[baseTop:baseTop+written])
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
	str, err := s.checkStringArg(args, 0, "find")
	if err != nil {
		return nil, err
	}
	pat, err := s.checkStringArg(args, 1, "find")
	if err != nil {
		return nil, err
	}
	initPos := 1
	if len(args) >= 3 && !args[2].IsNil() {
		iv, ok, err := s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
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
	str, err := s.checkStringArg(args, 0, "match")
	if err != nil {
		return nil, err
	}
	pat, err := s.checkStringArg(args, 1, "match")
	if err != nil {
		return nil, err
	}
	initPos := 1
	if len(args) >= 3 && !args[2].IsNil() {
		iv, ok, err := s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
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

// gmatchState is the Go payload of the gmatch iterator's state userdata (upvalue #3).
type gmatchState struct {
	it     *luastdlib.GMatchIter
	srcLen int
}

func nativeStringGMatch(s *State, args []Value) ([]Value, error) {
	str, err := s.checkStringArg(args, 0, "gmatch")
	if err != nil {
		return nil, err
	}
	pat, err := s.checkStringArg(args, 1, "gmatch")
	if err != nil {
		return nil, err
	}
	initPos := 1
	if len(args) >= 3 && !args[2].IsNil() {
		iv, ok, err := s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'gmatch' (number expected)")
		}
		initPos = int(iv)
	}
	var strVal Value
	var pinIDs []int
	defer func() {
		for _, id := range pinIDs {
			s.Unref(id)
		}
	}()
	if args[0].IsString() {
		strVal = args[0]
	} else {
		ls, ierr := s.TryIntern(str)
		if ierr != nil {
			return nil, ierr
		}
		strVal = StringValue(ls)
		str = ls.Str()
		if s.memLimit > 0 {
			pinIDs = append(pinIDs, s.Ref(strVal))
		}
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
		pat = ls.Str()
		if s.memLimit > 0 {
			pinIDs = append(pinIDs, s.Ref(patVal))
		}
	}
	if err := s.checkPreallocMem(s.gmatchPreallocBytes()); err != nil {
		return nil, err
	}
	it := luastdlib.GMatchWithCancel(str, pat, initPos, s.dynamicNativeCancelFunc())
	// Trace subject and pattern through immutable internal gcRefs so extracting upvalue #3
	// via debug.getupvalue(iter, 3) still accounts for both retained strings under memLimit
	// without exposing or allowing mutation via debug.getuservalue/setuservalue.
	stateUD, uerr := s.TryNewUserdata(&gmatchState{it: it, srcLen: len(str)}, 0)
	if uerr != nil {
		return nil, uerr
	}
	if err := s.SetUserdataGCRefs(s.asUserdata(stateUD), []Value{strVal, patVal}); err != nil {
		return nil, s.memError()
	}
	var selfVal Value
	fn := &NativeFunc{
		Name:        "gmatch_iter",
		NumUpvals:   3,
		Collectable: true,
		upvals:      []Value{strVal, patVal, stateUD},
		Fn: func(s *State, _ []Value) ([]Value, error) {
			var gs *gmatchState
			if uv, ok := s.nativeUpvalGet(selfVal, 2); ok && uv.IsUserdata() {
				gs, _ = s.UserdataData(s.asUserdata(uv)).(*gmatchState)
			}
			if gs == nil {
				return nil, fmt.Errorf("bad gmatch iterator state")
			}
			caps, ok, err := gs.it.Next()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, nil
			}
			return s.capturesToValuesRooted(caps, gs.srcLen)
		},
	}
	val, nerr := s.allocDynNative(fn)
	if nerr != nil {
		return nil, s.memError()
	}
	selfVal = val
	return []Value{val}, nil
}

func nativeStringGSub(s *State, args []Value) ([]Value, error) {
	str, err := s.checkStringArg(args, 0, "gsub")
	if err != nil {
		return nil, err
	}
	pat, err := s.checkStringArg(args, 1, "gsub")
	if err != nil {
		return nil, err
	}
	if len(args) < 3 {
		return nil, fmt.Errorf("bad argument #3 to 'gsub' (string/function/table expected)")
	}
	repl := args[2]
	maxRepl := -1
	if len(args) >= 4 && !args[3].IsNil() {
		iv, ok, err := s.coerceInt(args[3])
		if err != nil {
			return nil, err
		}
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
		outStr, count, err = luastdlib.GSubStringWithLimits(str, pat, s.FormatValue(repl), maxRepl, cancelFn, checkSize)
		replaced = count > 0
	case repl.IsTable():
		outStr, count, err = luastdlib.GSubTableWithLimits(str, pat, func(key luastdlib.CaptureVal) (string, bool, error) {
			kv, kerr := captureToValue(s, key, len(str))
			if kerr != nil {
				return "", false, kerr
			}
			var val Value
			t := s.asTable(repl)
			if herr := s.ensureLookupKeyHash(t, kv); herr != nil {
				return "", false, herr
			}
			val = t.RawGet(s, kv)
			if val.IsNil() && t.hasMeta() {
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
				return s.FormatValue(val), true, nil
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
				return s.FormatValue(res[0]), true, nil
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
	fmtStr, err := s.checkStringArg(args, 0, "pack")
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
			pargs[i] = luastdlib.PackStringVal(s.asString(a).Str())
		default:
			// Preserve an invalid PackKind so PackWithLimits rejects unsupported values iff consumed,
			// without rejecting unused trailing arguments.
			pargs[i] = luastdlib.PackValue{Kind: ^luastdlib.PackKind(0)}
		}
	}
	if err := s.preCoercePackArgs(fmtStr, pargs); err != nil {
		return nil, err
	}
	checkSize := func(n int64) error {
		return s.checkMem(n + 48)
	}
	var res string
	if cancelFn := s.nativeCancelFunc(); cancelFn != nil {
		res, err = s.packWithCancel(fmtStr, checkSize, cancelFn, pargs)
	} else {
		res, err = luastdlib.PackWithControls(fmtStr, checkSize, nil, pargs...)
	}
	if err != nil {
		return nil, err
	}
	ls, err := s.TryIntern(res)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

func nativeStringUnpack(s *State, args []Value) ([]Value, error) {
	fmtStr, err := s.checkStringArg(args, 0, "unpack")
	if err != nil {
		return nil, err
	}
	data, err := s.checkStringArg(args, 1, "unpack")
	if err != nil {
		return nil, err
	}
	initPos := int64(1)
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		initPos, ok, err = s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'unpack' (number expected)")
		}
	}
	// Pass 1: validate format/data and count decoded outputs with zero slice allocations,
	// enforcing memLimit and cancellation controls before allocating any result storage.
	cancelFn := s.nativeCancelFunc()
	runUnpack := func(emit func(luastdlib.PackValue) error) (int64, error) {
		if cancelFn != nil {
			return unpackEachWithCancel(fmtStr, data, initPos, cancelFn, emit)
		}
		return luastdlib.UnpackEachWithCancel(fmtStr, data, initPos, nil, emit)
	}
	var count int64
	var strBytes int64
	nextPos, err := runUnpack(func(pv luastdlib.PackValue) error {
		count++
		if count > 1_000_000 {
			return fmt.Errorf("too many results to unpack")
		}
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
		th := s.cur()
		savedTop := th.top
		baseTop := savedTop
		frames := th.framesSlice(s.arena)
		for fi := range th.nframe {
			cf := &frames[fi]
			if cl := s.frameClosure(cf); cl != nil {
				if end := cf.base + int(s.boundProtoOf(cl).maxStack); end > baseTop {
					baseTop = end
				}
			}
		}
		if err := th.ensureStack(s, baseTop+nOut+2); err != nil {
			return nil, err
		}
		written := 0
		defer func() {
			if written > 0 {
				fillNil(th.stackSlice(s.arena)[baseTop : baseTop+written])
			}
			th.top = savedTop
		}()
		_, err = runUnpack(func(pv luastdlib.PackValue) error {
			var v Value
			switch pv.Kind {
			case luastdlib.PackInt:
				v = IntValue(pv.Int)
			case luastdlib.PackFloat:
				v = FloatValue(pv.Flt)
			case luastdlib.PackString:
				str := pv.Str
				if len(str) > maxShortStringLen && len(str) < len(data) {
					var cerr error
					str, cerr = s.cloneStringWithCancel(str)
					if cerr != nil {
						return cerr
					}
				}
				ls, ierr := s.TryIntern(str)
				if ierr != nil {
					return ierr
				}
				v = StringValue(ls)
			}
			th.stackSlice(s.arena)[baseTop+written] = v
			written++
			th.top = baseTop + written
			return nil
		})
		if err != nil {
			return nil, err
		}
		th.stackSlice(s.arena)[baseTop+written] = IntValue(nextPos)
		written++
		th.top = baseTop + written
		out := make([]Value, written)
		copy(out, th.stackSlice(s.arena)[baseTop:baseTop+written])
		return out, nil
	}
	out := make([]Value, nOut)
	idx := 0
	_, err = runUnpack(func(pv luastdlib.PackValue) error {
		switch pv.Kind {
		case luastdlib.PackInt:
			out[idx] = IntValue(pv.Int)
		case luastdlib.PackFloat:
			out[idx] = FloatValue(pv.Flt)
		case luastdlib.PackString:
			str := pv.Str
			if len(str) > maxShortStringLen && len(str) < len(data) {
				var cerr error
				str, cerr = s.cloneStringWithCancel(str)
				if cerr != nil {
					return cerr
				}
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
	fmtStr, err := s.checkStringArg(args, 0, "packsize")
	if err != nil {
		return nil, err
	}
	cancelFn := s.nativeCancelFunc()
	if cancelFn != nil {
		if nul, cerr := indexByteWithCancel(fmtStr, 0, cancelFn); cerr != nil {
			return nil, cerr
		} else if nul >= 0 {
			fmtStr = fmtStr[:nul]
		}
	}
	sz, err := luastdlib.PackSizeWithCancel(fmtStr, cancelFn)
	if err != nil {
		if isUncatchableErr(err) {
			return nil, err
		}
		return nil, fmt.Errorf("bad argument #1 to 'packsize' (%s)", err.Error())
	}
	return []Value{IntValue(sz)}, nil
}

func nativeStringFormatStdlib(s *State, args []Value) ([]Value, error) {
	fmtStr, err := s.checkStringArg(args, 0, "format")
	if err != nil {
		return nil, err
	}
	hasStrSpec, err := containsByteWithCancel(fmtStr, 's', s.nativeCancelFunc())
	if err != nil {
		return nil, err
	}
	fargs := make([]luastdlib.FormatArg, len(args)-1)
	for i, a := range args[1:] {
		var lazyToStr func() (string, error)
		if hasStrSpec {
			val := a
			lazyToStr = func() (string, error) {
				return s.toStringValue(val)
			}
		}
		switch {
		case a.IsNil():
			fa := luastdlib.NilArg()
			fa.ToString = lazyToStr
			fargs[i] = fa
		case a.IsBool():
			fa := luastdlib.BoolArg(a.IsTrue())
			fa.ToString = lazyToStr
			fargs[i] = fa
		case a.IsInt():
			fa := luastdlib.IntArg(a.Int())
			fa.ToString = lazyToStr
			fargs[i] = fa
		case a.IsFloat():
			fa := luastdlib.FloatArg(a.Float())
			fa.ToString = lazyToStr
			fargs[i] = fa
		case a.IsString():
			fargs[i] = luastdlib.FormatArg{Kind: luastdlib.ArgString, Str: s.asString(a).Str(), Ptr: s.valueAddr(a), ToString: lazyToStr}
		default:
			fargs[i] = luastdlib.FormatArg{Kind: luastdlib.ArgPointer, Ptr: s.valueAddr(a), ToString: lazyToStr}
		}
	}
	if err := s.preCoerceFormatArgs(fmtStr, fargs); err != nil {
		return nil, err
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
	cancelFn := s.nativeCancelFunc()
	cps := make([]int64, len(args))
	for i, a := range args {
		if cancelFn != nil && ((i+1)&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		cp, ok, err := s.coerceInt(a)
		if err != nil {
			return nil, err
		}
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
	str, err := s.checkStringArg(args, 0, "codepoint")
	if err != nil {
		return nil, err
	}
	i := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok bool
		i, ok, err = s.coerceInt(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'codepoint'")
		}
	}
	j := i
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		j, ok, err = s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
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
		if count > 1_000_000 {
			return fmt.Errorf("string slice too long")
		}
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
		if cancelFn != nil && (len(out)&1023) == 0 {
			if cerr := cancelFn(); cerr != nil {
				return cerr
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func nativeUTF8Len(s *State, args []Value) ([]Value, error) {
	str, err := s.checkStringArg(args, 0, "len")
	if err != nil {
		return nil, err
	}
	i := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok bool
		i, ok, err = s.coerceInt(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'len'")
		}
	}
	j := int64(-1)
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		j, ok, err = s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
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

var (
	utf8CodesStrictIter = &NativeFunc{
		Name: "utf8_codes_iter",
		Fn:   makeUTF8CodesIterFn(false),
	}
	utf8CodesLaxIter = &NativeFunc{
		Name: "utf8_codes_iter",
		Fn:   makeUTF8CodesIterFn(true),
	}
)

func makeUTF8CodesIterFn(lax bool) func(*State, []Value) ([]Value, error) {
	return func(s *State, iargs []Value) ([]Value, error) {
		sv, err := s.checkStringArg(iargs, 0, "codes")
		if err != nil {
			return nil, err
		}
		pos := int64(0)
		if len(iargs) >= 2 && !iargs[1].IsNil() {
			var ok bool
			pos, ok, err = s.coerceInt(iargs[1])
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("bad argument #2 to 'codes' (number expected)")
			}
		}
		nextPos, cp, ok, err := luastdlib.NextWithCancel(sv, pos, lax, s.nativeCancelFunc())
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return []Value{IntValue(nextPos), IntValue(cp)}, nil
	}
}

func nativeUTF8Codes(s *State, args []Value) ([]Value, error) {
	str, err := s.checkStringArg(args, 0, "codes")
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
	iter := utf8CodesStrictIter
	if lax {
		iter = utf8CodesLaxIter
	}
	return []Value{s.NativeValue(iter), strVal, IntValue(0)}, nil
}

// nativeUTF8Offset implements Lua 5.5.1 two-return utf8.offset(s, n [, i]) -> start, end!
func nativeUTF8Offset(s *State, args []Value) ([]Value, error) {
	str, err := s.checkStringArg(args, 0, "offset")
	if err != nil {
		return nil, err
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'offset' (number expected)")
	}
	n, ok, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'offset' (number expected)")
	}
	i := int64(1)
	if n < 0 {
		i = int64(len(str)) + 1
	}
	if len(args) >= 3 && !args[2].IsNil() {
		i, ok, err = s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
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
	cancelFn  func() error
	written   int64
}

func (w *limitedStringWriter) pollCancel(n int) error {
	if w.cancelFn == nil || n <= 0 {
		return nil
	}
	const stride = 4096
	prev := w.written / stride
	w.written += int64(n)
	cur := w.written / stride
	for prev < cur {
		prev++
		if err := w.cancelFn(); err != nil {
			return err
		}
	}
	return nil
}

func (w *limitedStringWriter) Write(p []byte) (int, error) {
	if w.checkSize != nil {
		if err := w.checkSize(int64(w.sb.Len() + len(p))); err != nil {
			return 0, err
		}
	}
	if w.cancelFn != nil && len(p) > 4096 {
		total := 0
		for off := 0; off < len(p); off += 4096 {
			end := min(off+4096, len(p))
			if err := w.pollCancel(end - off); err != nil {
				return total, err
			}
			n, err := w.sb.Write(p[off:end])
			total += n
			if err != nil {
				return total, err
			}
		}
		return total, nil
	}
	if err := w.pollCancel(len(p)); err != nil {
		return 0, err
	}
	return w.sb.Write(p)
}

func (w *limitedStringWriter) WriteString(s string) (int, error) {
	if w.checkSize != nil {
		if err := w.checkSize(int64(w.sb.Len() + len(s))); err != nil {
			return 0, err
		}
	}
	if w.cancelFn != nil && len(s) > 4096 {
		total := 0
		for off := 0; off < len(s); off += 4096 {
			end := min(off+4096, len(s))
			if err := w.pollCancel(end - off); err != nil {
				return total, err
			}
			n, err := w.sb.WriteString(s[off:end])
			total += n
			if err != nil {
				return total, err
			}
		}
		return total, nil
	}
	if err := w.pollCancel(len(s)); err != nil {
		return 0, err
	}
	return w.sb.WriteString(s)
}

func nativeStringDump(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsClosure() {
		return nil, fmt.Errorf("bad argument #1 to 'dump' (Lua function expected)")
	}
	strip := len(args) >= 2 && !args[1].IsFalsy()
	p := s.boundProtoOf(s.asClosure(args[0])).proto
	cancelFn := s.nativeCancelFunc()
	if s.abortErr != nil {
		return nil, s.abortErr
	}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			s.abortErr = err
			return nil, err
		}
	}
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
	w := limitedStringWriter{checkSize: checkSize, cancelFn: cancelFn}
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
