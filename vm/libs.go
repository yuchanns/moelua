package vm

import (
	"fmt"
	"math"

	"github.com/yuchanns/moelua/internal/luastdlib"
)

// LibMask selects which Lua 5.5.1 standard libraries are opened in a State or Template,
// mirroring PUC-Rio Lua 5.5's luaL_openselectedlibs bitmask with an additional sandboxed
// package option (LibPackageSafe).
type LibMask uint32

const (
	// LibNone opens no standard libraries (an empty global environment, like lua_newstate).
	LibNone LibMask = 0

	// LibBase opens the core Lua 5.5 base globals (_G, _VERSION, print, assert, error,
	// pcall, xpcall, type, tostring, tonumber, select, raw*, set/getmetatable,
	// collectgarbage, next, pairs, ipairs, warn, load) without filesystem loaders.
	LibBase LibMask = 1 << iota

	// LibBaseFile opens filesystem script loaders in base (loadfile, dofile).
	LibBaseFile

	// LibPackage opens the full package library and require() (preload, OS lua/c searchers,
	// package.searchpath, package.loadlib, and LUA_PATH/LUA_CPATH environment variables).
	LibPackage

	// LibPackageSafe opens a sandboxed package library and require() with ONLY in-memory
	// package.preload (plus host-registered AddFS / AddSearcher searchers): omits OS
	// filesystem searchers, package.loadlib, package.searchpath, package.cpath, and ignores
	// LUA_PATH/LUA_CPATH environment variables. If both LibPackage and LibPackageSafe are
	// set, LibPackage takes precedence.
	LibPackageSafe

	// LibCoroutine opens the coroutine library.
	LibCoroutine

	// LibString opens the string library and installs the string type metatable
	// (__index = string plus Lua 5.5 string arithmetic metamethods).
	LibString

	// LibUTF8 opens the utf8 library.
	LibUTF8

	// LibTable opens the table library.
	LibTable

	// LibMath opens the math library.
	LibMath

	// LibIO opens the io library and binds stdin/stdout/stderr file handles.
	LibIO

	// LibOS opens the os library.
	LibOS

	// LibDebug opens the debug library and registry._HOOKKEY.
	LibDebug

	// LibAll opens the complete standard Lua 5.5.1 library (default for NewState).
	LibAll = LibBase | LibBaseFile | LibPackage | LibCoroutine | LibString | LibUTF8 | LibTable | LibMath | LibIO | LibOS | LibDebug

	// LibSafe opens the compute-safe sandbox subset (base without loadfile/dofile,
	// coroutine, string, utf8, table, math). Combine with LibPackageSafe to also enable
	// require() over Preload and WithFS without OS filesystem access.
	LibSafe = LibBase | LibCoroutine | LibString | LibUTF8 | LibTable | LibMath

	libMaskValid = LibAll | LibPackageSafe
)

func normalizeLibMask(mask LibMask) LibMask {
	mask &= libMaskValid
	if mask&LibPackage != 0 {
		mask &^= LibPackageSafe
	}
	return mask
}

// OpenLibs opens the selected standard libraries on s. Libraries already opened on s are
// skipped (idempotent), except upgrading LibPackageSafe to LibPackage adds the full OS
// package searchers and helpers.
func (s *State) OpenLibs(mask LibMask) {
	mask = normalizeLibMask(mask)
	toOpen := mask &^ s.openedLibs
	if toOpen == 0 || s.memExceeded {
		return
	}

	if toOpen&LibBase != 0 {
		s.initBaseLib()
		if s.memExceeded {
			return
		}
		s.syncLoadedLib("_G", TableValue(s.GlobalEnv()))
		if s.memExceeded {
			return
		}
		s.openedLibs |= LibBase
	}
	if toOpen&LibBaseFile != 0 {
		s.initBaseFileLib()
		if s.memExceeded {
			return
		}
		s.openedLibs |= LibBaseFile
	}
	if toOpen&LibMath != 0 && !s.openStdTableLib(LibMath, "math", s.initMathLib) {
		return
	}
	if toOpen&LibTable != 0 && !s.openStdTableLib(LibTable, "table", s.initTableLib) {
		return
	}
	if toOpen&LibString != 0 && !s.openStdTableLib(LibString, "string", s.initStringLib) {
		return
	}
	if toOpen&LibUTF8 != 0 && !s.openStdTableLib(LibUTF8, "utf8", s.initUTF8Lib) {
		return
	}
	if toOpen&LibCoroutine != 0 && !s.openStdTableLib(LibCoroutine, "coroutine", s.initCoroLib) {
		return
	}
	if toOpen&LibDebug != 0 && !s.openStdTableLib(LibDebug, "debug", s.initDebugLib) {
		return
	}
	if toOpen&LibIO != 0 && !s.openStdTableLib(LibIO, "io", s.initIOLib) {
		return
	}
	if toOpen&LibOS != 0 && !s.openStdTableLib(LibOS, "os", s.initOSLib) {
		return
	}
	if toOpen&LibPackage != 0 {
		if s.initPackageLibWithMode(true, true) == nil || s.memExceeded {
			return
		}
		s.openedLibs = (s.openedLibs &^ LibPackageSafe) | LibPackage
		if !s.intern.bootstrapping && !s.isNoEnv() {
			s.applyPackageEnv()
		}
	} else if toOpen&LibPackageSafe != 0 && s.openedLibs&LibPackage == 0 {
		if s.initPackageLibWithMode(false, true) == nil || s.memExceeded {
			return
		}
		s.openedLibs |= LibPackageSafe
	}
}

func (s *State) pinRuntime(v Value) int {
	if !v.IsGCObject() {
		return 0
	}
	return s.Ref(v)
}

func (s *State) unpinRuntime(id int) {
	if id != 0 {
		s.Unref(id)
	}
}

func (s *State) newLibTable(nseq, nrec int) *Table {
	if s.memLimit > 0 {
		t, err := s.TryNewTable(nseq, nrec)
		if err != nil {
			s.memExceeded = true
			return nil
		}
		return t
	}
	return s.NewTable(nseq, nrec)
}

func (s *State) openStdTableLib(bit LibMask, name string, initFn func() *Table) bool {
	if s.memExceeded {
		return false
	}
	var val Value
	fromLoaded := false
	if existing := s.lookupLoadedLib(name); !existing.IsFalsy() {
		val = existing
		fromLoaded = true
	} else {
		tbl := initFn()
		if s.memExceeded || tbl == nil {
			return false
		}
		val = TableValue(tbl)
	}
	pinID := s.Ref(val)
	defer s.Unref(pinID)

	prevGlobal := s.GetGlobal(name)
	s.SetGlobal(name, val)
	if s.memExceeded {
		return false
	}
	if !fromLoaded {
		s.syncLoadedLib(name, val)
		if s.memExceeded {
			s.SetGlobal(name, prevGlobal)
			return false
		}
	}
	s.openedLibs |= bit
	return true
}

func (s *State) lookupLoadedLib(name string) Value {
	reg := s.registry()
	if reg == nil {
		return NilValue()
	}
	loadedKey, ok := s.LookupString("_LOADED")
	if !ok {
		return NilValue()
	}
	lv := reg.RawGetStr(s, loadedKey)
	if !lv.IsTable() {
		return NilValue()
	}
	nameKey, ok := s.LookupString(name)
	if !ok {
		return NilValue()
	}
	return s.asTable(lv).RawGetStr(s, nameKey)
}

var (
	stdPreloadBase        = &NativeFunc{Name: "luaopen_base", Fn: nativePreloadBase}
	stdPreloadBaseFull    = &NativeFunc{Name: "luaopen_base", Fn: nativePreloadBaseFull}
	stdPreloadPackage     = &NativeFunc{Name: "luaopen_package", Fn: nativePreloadPackage}
	stdPreloadPackageSafe = &NativeFunc{Name: "luaopen_package", Fn: nativePreloadPackageSafe}
	stdPreloadCoroutine   = &NativeFunc{Name: "luaopen_coroutine", Fn: nativePreloadCoroutine}
	stdPreloadString      = &NativeFunc{Name: "luaopen_string", Fn: nativePreloadString}
	stdPreloadUTF8        = &NativeFunc{Name: "luaopen_utf8", Fn: nativePreloadUTF8}
	stdPreloadTable       = &NativeFunc{Name: "luaopen_table", Fn: nativePreloadTable}
	stdPreloadMath        = &NativeFunc{Name: "luaopen_math", Fn: nativePreloadMath}
	stdPreloadIO          = &NativeFunc{Name: "luaopen_io", Fn: nativePreloadIO}
	stdPreloadOS          = &NativeFunc{Name: "luaopen_os", Fn: nativePreloadOS}
	stdPreloadDebug       = &NativeFunc{Name: "luaopen_debug", Fn: nativePreloadDebug}
)

func nativePreloadBase(s *State, _ []Value) ([]Value, error) {
	if s.openedLibs&LibBase == 0 {
		s.initBaseLib()
		if err := s.ConsumeMemErr(); err != nil {
			return nil, err
		}
		s.openedLibs |= LibBase
	}
	return []Value{TableValue(s.GlobalEnv())}, nil
}

func nativePreloadBaseFull(s *State, _ []Value) ([]Value, error) {
	if s.openedLibs&LibBase == 0 {
		s.initBaseLib()
		if err := s.ConsumeMemErr(); err != nil {
			return nil, err
		}
		s.openedLibs |= LibBase
	}
	if s.openedLibs&LibBaseFile == 0 {
		s.initBaseFileLib()
		if err := s.ConsumeMemErr(); err != nil {
			return nil, err
		}
		s.openedLibs |= LibBaseFile
	}
	return []Value{TableValue(s.GlobalEnv())}, nil
}

func nativePreloadPackage(s *State, _ []Value) ([]Value, error) {
	pkg := s.initPackageLibWithMode(true, false)
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, s.memError()
	}
	s.openedLibs = (s.openedLibs &^ LibPackageSafe) | LibPackage
	if !s.intern.bootstrapping && !s.isNoEnv() {
		s.applyPackageEnv()
		if err := s.ConsumeMemErr(); err != nil {
			return nil, err
		}
	}
	return []Value{TableValue(pkg)}, nil
}

func nativePreloadPackageSafe(s *State, _ []Value) ([]Value, error) {
	pkg := s.initPackageLibWithMode(false, false)
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, s.memError()
	}
	if s.openedLibs&LibPackage == 0 {
		s.openedLibs |= LibPackageSafe
	}
	return []Value{TableValue(pkg)}, nil
}

func nativePreloadCoroutine(s *State, _ []Value) ([]Value, error) {
	tbl := s.initCoroLib()
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(tbl)}, nil
}

func nativePreloadString(s *State, _ []Value) ([]Value, error) {
	tbl := s.initStringLib()
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(tbl)}, nil
}

func nativePreloadUTF8(s *State, _ []Value) ([]Value, error) {
	tbl := s.initUTF8Lib()
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(tbl)}, nil
}

func nativePreloadTable(s *State, _ []Value) ([]Value, error) {
	tbl := s.initTableLib()
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(tbl)}, nil
}

func nativePreloadMath(s *State, _ []Value) ([]Value, error) {
	tbl := s.initMathLib()
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(tbl)}, nil
}

func nativePreloadIO(s *State, _ []Value) ([]Value, error) {
	tbl := s.initIOLib()
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(tbl)}, nil
}

func nativePreloadOS(s *State, _ []Value) ([]Value, error) {
	tbl := s.initOSLib()
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(tbl)}, nil
}

func nativePreloadDebug(s *State, _ []Value) ([]Value, error) {
	tbl := s.initDebugLib()
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(tbl)}, nil
}

// PreloadLibs registers lazy loaders for the selected standard libraries into
// package.preload (registry._PRELOAD) without populating globals, mirroring the
// preload bitmask of PUC-Rio Lua 5.5's luaL_openselectedlibs(L, load, preload).
// Libraries already opened in s are skipped.
func (s *State) PreloadLibs(mask LibMask) {
	mask = normalizeLibMask(mask)
	toPreload := mask &^ s.openedLibs
	if toPreload == 0 {
		return
	}
	preload := s.PreloadTable()
	if preload == nil {
		return
	}
	set := func(name string, nf *NativeFunc) {
		preload.RawSetStr(s, s.intern.Intern(name), s.allocStaticNative(nf))
	}
	if toPreload&LibBase != 0 {
		if mask&LibBaseFile != 0 {
			set("_G", stdPreloadBaseFull)
		} else {
			set("_G", stdPreloadBase)
		}
	}
	if s.openedLibs&(LibPackage|LibPackageSafe) == 0 {
		if toPreload&LibPackage != 0 {
			set("package", stdPreloadPackage)
		} else if toPreload&LibPackageSafe != 0 {
			set("package", stdPreloadPackageSafe)
		}
	}
	if toPreload&LibCoroutine != 0 {
		set("coroutine", stdPreloadCoroutine)
	}
	if toPreload&LibString != 0 {
		set("string", stdPreloadString)
	}
	if toPreload&LibUTF8 != 0 {
		set("utf8", stdPreloadUTF8)
	}
	if toPreload&LibTable != 0 {
		set("table", stdPreloadTable)
	}
	if toPreload&LibMath != 0 {
		set("math", stdPreloadMath)
	}
	if toPreload&LibIO != 0 {
		set("io", stdPreloadIO)
	}
	if toPreload&LibOS != 0 {
		set("os", stdPreloadOS)
	}
	if toPreload&LibDebug != 0 {
		set("debug", stdPreloadDebug)
	}
}

func (s *State) isNoEnv() bool {
	reg := s.registry()
	if reg == nil {
		return false
	}
	key, ok := s.LookupString("LUA_NOENV")
	if !ok {
		return false
	}
	return !reg.RawGetStr(s, key).IsFalsy()
}

// syncLoadedLib records an opened stdlib in registry._LOADED if package.loaded already exists.
func (s *State) syncLoadedLib(name string, v Value) {
	reg := s.registry()
	if reg == nil {
		return
	}
	key, ok := s.LookupString("_LOADED")
	if !ok {
		return
	}
	if lv := reg.RawGetStr(s, key); lv.IsTable() {
		s.asTable(lv).RawSetStr(s, s.intern.Intern(name), v)
	}
}

// LoadedTable returns registry._LOADED (creating it on demand, matching luaL_getsubtable).
func (s *State) LoadedTable() *Table {
	reg := s.registry()
	if reg == nil {
		return nil
	}
	key := s.intern.Intern("_LOADED")
	if v := reg.RawGetStr(s, key); v.IsTable() {
		return s.asTable(v)
	}
	t := s.NewTable(0, 8)
	reg.RawSetStr(s, key, TableValue(t))
	return t
}

// RequireLib implements Lua 5.5 luaL_requiref(L, modname, openf, glb): if package.loaded[name]
// is absent or falsy, invokes loader(name), caches the returned module (defaulting to true
// when nil/false) in registry._LOADED[name], and sets _G[name] when global is true.
func (s *State) RequireLib(name string, loader *NativeFunc, global bool) (Value, error) {
	if loader == nil {
		return NilValue(), fmt.Errorf("moelua: RequireLib %q requires a non-nil loader", name)
	}
	nameLS, err := s.TryIntern(name)
	if err != nil {
		return NilValue(), err
	}
	nameID := s.Ref(StringValue(nameLS))
	defer s.Unref(nameID)

	loaded := s.LoadedTable()
	if loaded == nil {
		return NilValue(), fmt.Errorf("moelua: registry unavailable")
	}
	if err := s.ConsumeMemErr(); err != nil {
		return NilValue(), err
	}

	mod := loaded.RawGetStr(s, nameLS)
	if mod.IsFalsy() {
		fnVal, err := s.TryAllocNative(loader)
		if err != nil {
			return NilValue(), err
		}
		fnID := s.Ref(fnVal)
		rets, callErr := s.Call(fnVal, StringValue(nameLS))
		s.Unref(fnID)
		if callErr != nil {
			return NilValue(), callErr
		}
		if len(rets) > 0 && !rets[0].IsFalsy() {
			mod = rets[0]
		} else {
			mod = TrueValue()
		}
		modID := s.Ref(mod)
		defer s.Unref(modID)
		loaded = s.LoadedTable()
		loaded.RawSetStr(s, nameLS, mod)
		if err := s.ConsumeMemErr(); err != nil {
			return NilValue(), err
		}
	}

	if global {
		modID := s.Ref(mod)
		defer s.Unref(modID)
		if env := s.GlobalEnv(); env != nil {
			env.RawSetStr(s, nameLS, mod)
			if err := s.ConsumeMemErr(); err != nil {
				return NilValue(), err
			}
		}
	}
	return mod, nil
}

func (s *State) initBaseLib() {
	env := s.GlobalEnv()
	s.SetGlobal("_G", TableValue(env))
	s.SetGlobal("_VERSION", StringValue(s.intern.Intern("Lua 5.5")))

	s.registerFunc(env, "print", intrNone, nativePrint)
	s.registerFunc(env, "assert", intrNone, nativeAssert)
	s.registerFunc(env, "error", intrNone, nativeError)
	s.registerFunc(env, "pcall", intrPCall, nativePCall)
	s.registerFunc(env, "xpcall", intrXPCall, nativeXPCall)
	s.registerFunc(env, "type", intrType, nativeType)
	s.registerFunc(env, "tostring", intrNone, nativeToString)
	s.registerFunc(env, "tonumber", intrNone, nativeToNumber)
	s.registerFunc(env, "select", intrSelect, nativeSelect)
	s.registerFunc(env, "rawget", intrRawGet, nativeRawGet)
	s.registerFunc(env, "rawset", intrRawSet, nativeRawSet)
	s.registerFunc(env, "rawequal", intrNone, nativeRawEqual)
	s.registerFunc(env, "rawlen", intrRawLen, nativeRawLen)
	s.registerFunc(env, "setmetatable", intrNone, nativeSetMetatable)
	s.registerFunc(env, "getmetatable", intrNone, nativeGetMetatable)
	s.registerFunc(env, "collectgarbage", intrNone, nativeCollectGarbage)

	s.builtinNext = s.registerFunc(env, "next", intrNext, nativeNext)
	s.builtinIPairsIter = lookupStaticNative(registerStaticNative(&NativeFunc{Name: "ipairs_iter", Fn: nativeIPairsIter, Intrinsic: intrIPairsIter}))
	s.registerFunc(env, "pairs", intrPairs, nativePairs)
	s.registerFunc(env, "ipairs", intrNone, nativeIPairs)

	s.registerFunc(env, "warn", intrNone, nativeWarn)
	s.registerFunc(env, "load", intrNone, nativeLoad)
}

func (s *State) initBaseFileLib() {
	env := s.GlobalEnv()
	s.registerFunc(env, "loadfile", intrNone, nativeLoadFile)
	s.registerFunc(env, "dofile", intrDoFile, nativeDoFile)
}

func (s *State) initMathLib() *Table {
	mathTbl := s.newLibTable(0, 32)
	if mathTbl == nil {
		return nil
	}
	pin := s.pinRuntime(TableValue(mathTbl))
	defer s.unpinRuntime(pin)
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
	return mathTbl
}

func (s *State) initTableLib() *Table {
	tableTbl := s.newLibTable(0, 12)
	if tableTbl == nil {
		return nil
	}
	pin := s.pinRuntime(TableValue(tableTbl))
	defer s.unpinRuntime(pin)
	s.registerFunc(tableTbl, "create", intrNone, nativeTableCreate)
	s.registerFunc(tableTbl, "insert", intrTableInsert, nativeTableInsert)
	s.registerFunc(tableTbl, "remove", intrTableRemove, nativeTableRemove)
	s.registerFunc(tableTbl, "concat", intrNone, nativeTableConcat)
	s.registerFunc(tableTbl, "move", intrNone, nativeTableMove)
	s.registerFunc(tableTbl, "pack", intrNone, nativeTablePack)
	s.registerFunc(tableTbl, "unpack", intrNone, nativeTableUnpack)
	s.registerFunc(tableTbl, "sort", intrNone, nativeTableSort)
	return tableTbl
}

func (s *State) initStringLib() *Table {
	strTbl := s.newLibTable(0, 20)
	if strTbl == nil {
		return nil
	}
	pinStr := s.pinRuntime(TableValue(strTbl))
	defer s.unpinRuntime(pinStr)

	s.registerFunc(strTbl, "len", intrStringLen, nativeStringLen)
	s.registerFunc(strTbl, "byte", intrStringByte, nativeStringByte)
	s.registerFunc(strTbl, "char", intrNone, nativeStringChar)
	s.registerFunc(strTbl, "sub", intrNone, nativeStringSub)
	s.registerFunc(strTbl, "rep", intrNone, nativeStringRep)
	s.registerFunc(strTbl, "reverse", intrNone, nativeStringReverse)
	s.registerFunc(strTbl, "lower", intrNone, nativeStringLower)
	s.registerFunc(strTbl, "upper", intrNone, nativeStringUpper)
	s.registerFunc(strTbl, "format", intrNone, nativeStringFormatStdlib)
	s.registerFunc(strTbl, "find", intrNone, nativeStringFind)
	s.registerFunc(strTbl, "match", intrNone, nativeStringMatch)
	s.registerFunc(strTbl, "gmatch", intrNone, nativeStringGMatch)
	s.registerFunc(strTbl, "gsub", intrNone, nativeStringGSub)
	s.registerFunc(strTbl, "pack", intrNone, nativeStringPack)
	s.registerFunc(strTbl, "unpack", intrNone, nativeStringUnpack)
	s.registerFunc(strTbl, "packsize", intrNone, nativeStringPackSize)
	s.registerFunc(strTbl, "dump", intrNone, nativeStringDump)
	if s.memExceeded {
		return nil
	}

	strMeta := s.newLibTable(0, 12)
	if strMeta == nil {
		return nil
	}
	pinMeta := s.pinRuntime(TableValue(strMeta))
	defer s.unpinRuntime(pinMeta)
	strMeta.RawSetStr(s, s.intern.tms[TMIndex], TableValue(strTbl))
	if s.memExceeded {
		return nil
	}
	s.initStringArithMetamethods(strMeta)
	if s.memExceeded {
		return nil
	}
	strMeta.markProto(s)
	s.setStringMeta(strMeta)
	return strTbl
}

func (s *State) initUTF8Lib() *Table {
	utf8Tbl := s.newLibTable(0, 8)
	if utf8Tbl == nil {
		return nil
	}
	pin := s.pinRuntime(TableValue(utf8Tbl))
	defer s.unpinRuntime(pin)
	utf8Tbl.RawSetStr(s, s.intern.Intern("charpattern"), StringValue(s.intern.Intern(luastdlib.CharPattern)))
	s.registerFunc(utf8Tbl, "char", intrNone, nativeUTF8Char)
	s.registerFunc(utf8Tbl, "codepoint", intrNone, nativeUTF8Codepoint)
	s.registerFunc(utf8Tbl, "len", intrNone, nativeUTF8Len)
	s.registerFunc(utf8Tbl, "codes", intrNone, nativeUTF8Codes)
	s.registerFunc(utf8Tbl, "offset", intrNone, nativeUTF8Offset)
	registerStaticNative(utf8CodesStrictIter)
	registerStaticNative(utf8CodesLaxIter)
	return utf8Tbl
}

func (s *State) initPackageLibWithMode(allowOS, setGlobal bool) *Table {
	reg := s.registry()
	var loaded, preload *Table
	if lv := reg.RawGetStr(s, s.intern.Intern("_LOADED")); lv.IsTable() {
		loaded = s.asTable(lv)
	} else {
		loaded = s.newLibTable(0, 16)
		if loaded == nil {
			return nil
		}
	}
	pinLoaded := s.pinRuntime(TableValue(loaded))
	defer s.unpinRuntime(pinLoaded)

	if pv := reg.RawGetStr(s, s.intern.Intern("_PRELOAD")); pv.IsTable() {
		preload = s.asTable(pv)
	} else {
		preload = s.newLibTable(0, 8)
		if preload == nil {
			return nil
		}
	}
	pinPreload := s.pinRuntime(TableValue(preload))
	defer s.unpinRuntime(pinPreload)

	// Preserve existing package table if upgrading from LibPackageSafe -> LibPackage.
	pkg := s.pkgTbl()
	if pkg == nil {
		pkg = s.newLibTable(0, 10)
		if pkg == nil {
			return nil
		}
	}
	pinPkg := s.pinRuntime(TableValue(pkg))
	defer s.unpinRuntime(pinPkg)
	if s.memExceeded {
		return nil
	}

	for _, name := range []string{"_G", "math", "table", "string", "os", "io", "coroutine", "debug", "utf8"} {
		if v := s.GetGlobal(name); !v.IsNil() {
			if loaded.RawGetStr(s, s.intern.Intern(name)).IsNil() {
				loaded.RawSetStr(s, s.intern.Intern(name), v)
				if s.memExceeded {
					return nil
				}
			}
		}
	}

	pkg.RawSetStr(s, s.intern.Intern("loaded"), TableValue(loaded))
	if !s.memExceeded {
		pkg.RawSetStr(s, s.intern.Intern("preload"), TableValue(preload))
	}
	if !s.memExceeded && pkg.RawGetStr(s, s.intern.Intern("path")).IsNil() {
		pkg.RawSetStr(s, s.intern.Intern("path"), StringValue(s.intern.Intern(defaultLuaPath)))
	}
	if !s.memExceeded {
		pkg.RawSetStr(s, s.intern.Intern("config"), StringValue(s.intern.Intern("/\n;\n?\n!\n-\n")))
	}
	if s.memExceeded {
		return nil
	}

	var searchers *Table
	if sv := pkg.RawGetStr(s, s.intern.Intern("searchers")); sv.IsTable() {
		searchers = s.asTable(sv)
	} else {
		searchers = s.newLibTable(4, 0)
		if searchers == nil {
			return nil
		}
		pinSearchers := s.pinRuntime(TableValue(searchers))
		defer s.unpinRuntime(pinSearchers)
		searchers.RawSetInt(s, 1, s.allocStaticNative(&NativeFunc{Name: "searcher_preload", Fn: nativeSearcherPreload}))
		if !s.memExceeded {
			pkg.RawSetStr(s, s.intern.Intern("searchers"), TableValue(searchers))
		}
	}
	if s.memExceeded {
		return nil
	}

	if allowOS {
		pkg.RawSetStr(s, s.intern.Intern("cpath"), StringValue(s.intern.Intern(defaultLuaCPath)))
		addedOS := false
		var osStartIdx int64
		if !s.pkgAllowOS {
			// Append OS searchers after preload (+ any host AddFS searchers already installed).
			osStartIdx = searchers.Len(s) + 1
			searchers.RawSetInt(s, osStartIdx, s.allocStaticNative(&NativeFunc{Name: "searcher_lua", Fn: nativeSearcherLua}))
			searchers.RawSetInt(s, osStartIdx+1, s.allocStaticNative(&NativeFunc{Name: "searcher_c", Fn: nativeSearcherC}))
			addedOS = true
		}
		s.registerFunc(pkg, "searchpath", intrNone, nativePackageSearchPath)
		s.registerFunc(pkg, "loadlib", intrNone, nativePackageLoadLib)
		if s.memExceeded {
			if addedOS {
				searchers.RawSetInt(s, osStartIdx, NilValue())
				searchers.RawSetInt(s, osStartIdx+1, NilValue())
			}
			return nil
		}
		s.pkgAllowOS = true
	}

	loaded.RawSetStr(s, s.intern.Intern("package"), TableValue(pkg))
	reg.RawSetStr(s, s.intern.Intern("_LOADED"), TableValue(loaded))
	reg.RawSetStr(s, s.intern.Intern("_PRELOAD"), TableValue(preload))
	s.registerFunc(s.GlobalEnv(), "require", intrNone, nativeRequire)
	if setGlobal {
		s.SetGlobal("package", TableValue(pkg))
	}
	if s.memExceeded {
		return nil
	}
	s.pkgRef = pkg.self
	return pkg
}
