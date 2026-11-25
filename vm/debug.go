package vm

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/yuchanns/moelua/bytecode"
)

// getFuncLine decodes the Lua 5.5 delta-encoded LineInfo + AbsLineInfo for pc
// (faithful port of ldebug.c:luaG_getfuncline).
func getFuncLine(p *bytecode.Proto, pc int) int {
	if p == nil || len(p.LineInfo) == 0 || pc < 0 || pc >= len(p.LineInfo) {
		return -1
	}
	// Find latest AbsLineInfo with abs.PC <= pc.
	basePC := 0
	baseLine := p.LineDefined
	if len(p.AbsLineInfo) > 0 && pc >= int(p.AbsLineInfo[0].PC) {
		lo, hi := 0, len(p.AbsLineInfo)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if int(p.AbsLineInfo[mid].PC) <= pc {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		basePC = int(p.AbsLineInfo[lo].PC)
		if basePC < 0 {
			basePC = 0
		}
		baseLine = int(p.AbsLineInfo[lo].Line)
		for i := basePC + 1; i <= pc; i++ {
			baseLine += int(p.LineInfo[i])
		}
		return baseLine
	}
	for i := 0; i <= pc; i++ {
		if p.LineInfo[i] == -128 { // ABSLINEINFO sentinel
			continue
		}
		baseLine += int(p.LineInfo[i])
	}
	return baseLine
}

// shortSrc implements Lua's luaO_chunkid for debug.getinfo().short_src.
func shortSrc(source string) string {
	const bufLen = 60
	if source == "" {
		return "[string \"\"]"
	}
	if source[0] == '=' {
		s := source[1:]
		if len(s) >= bufLen {
			return s[:bufLen-1]
		}
		return s
	}
	if source[0] == '@' {
		s := source[1:]
		if len(s) >= bufLen {
			return "..." + s[len(s)-(bufLen-4):]
		}
		return s
	}
	// Literal string chunk
	nl := strings.IndexByte(source, '\n')
	truncated := nl >= 0
	line := source
	if truncated {
		line = source[:nl]
	}
	maxLen := bufLen - len(`[string "..."]`) - 1
	if len(line) > maxLen {
		line = line[:maxLen]
		truncated = true
	}
	if truncated {
		return fmt.Sprintf(`[string "%s..."]`, line)
	}
	return fmt.Sprintf(`[string "%s"]`, line)
}

func (s *State) initDebugLib() {
	dbg := s.NewTable(0, 16)
	s.registerFunc(dbg, "getinfo", intrNone, nativeDebugGetInfo)
	if v := dbg.RawGetStr(s.intern.Intern("getinfo")); v.IsNative() {
		s.builtinDebugGetInfo = v.AsNative()
	}
	s.registerFunc(dbg, "traceback", intrNone, nativeDebugTraceback)
	s.registerFunc(dbg, "getlocal", intrNone, nativeDebugGetLocal)
	s.registerFunc(dbg, "setlocal", intrNone, nativeDebugSetLocal)
	s.registerFunc(dbg, "getupvalue", intrNone, nativeDebugGetUpvalue)
	s.registerFunc(dbg, "setupvalue", intrNone, nativeDebugSetUpvalue)
	s.registerFunc(dbg, "upvalueid", intrNone, nativeDebugUpvalueID)
	s.registerFunc(dbg, "upvaluejoin", intrNone, nativeDebugUpvalueJoin)
	s.registerFunc(dbg, "getmetatable", intrNone, nativeDebugGetMetatable)
	s.registerFunc(dbg, "setmetatable", intrNone, nativeDebugSetMetatable)
	s.registerFunc(dbg, "getregistry", intrNone, nativeDebugGetRegistry)
	s.registerFunc(dbg, "getuservalue", intrNone, nativeDebugGetUserValue)
	s.registerFunc(dbg, "setuservalue", intrNone, nativeDebugSetUserValue)
	s.registerFunc(dbg, "sethook", intrNone, nativeDebugSetHook)
	s.registerFunc(dbg, "gethook", intrNone, nativeDebugGetHook)
	s.registerFunc(dbg, "debug", intrNone, nativeDebugDebug)
	s.SetGlobal("debug", TableValue(dbg))

	// Registry _HOOKKEY weak-key table matching Lua 5.5 ldblib.c (db.lua:328).
	hookKey := s.NewDictTable(4)
	hkMeta := s.NewTable(0, 1)
	hkMeta.RawSetStr(s, s.intern.tms[TMMode], StringValue(s.intern.Intern("k")))
	hookKey.meta = hkMeta
	s.registry.RawSetStr(s, s.intern.Intern("_HOOKKEY"), TableValue(hookKey))
}

func optThreadArg(s *State, args []Value) (*Thread, []Value) {
	if len(args) > 0 && args[0].IsThread() {
		return args[0].AsThread(), args[1:]
	}
	return s.cur, args
}

func (s *State) formatTraceback(th *Thread, msg string, hasMsg bool, level int) string {
	var sb strings.Builder
	if hasMsg {
		sb.WriteString(msg)
		sb.WriteByte('\n')
	}
	sb.WriteString("stack traceback:")
	if level < 0 {
		return sb.String()
	}
	var entries []string
	// Level 0 on a suspended coroutine is [C]: in function 'yield';
	// Level 0 on a dead errored coroutine that died in a native call (e.g. error()) is that native.
	if th != s.cur {
		if th.status == ThreadSuspended {
			entries = append(entries, "\n\t[C]: in function 'yield'")
		} else if th.status == ThreadDead && !th.errObj.IsNil() && th.lastErrNative.IsNative() {
			entries = append(entries, fmt.Sprintf("\n\t[C]: in function '%s'", th.lastErrNative.AsNative().Name))
		}
	}
	for i := th.nframe - 1; i >= 0; i-- {
		cf := &th.frames[i]
		if cf.kind != frameLua {
			cname := "pcall"
			if cf.kind == frameXPCall {
				cname = "xpcall"
			} else if cf.kind == frameNative && cf.nativeFn.IsNative() {
				cname = cf.nativeFn.AsNative().Name
			}
			entries = append(entries, fmt.Sprintf("\n\t[C]: in function '%s'", cname))
			continue
		}
		if cf.cl == nil {
			continue
		}
		p := cf.cl.bp.proto
		pc := cf.pc
		if !cf.atCurrentPC && pc > 0 {
			pc--
		}
		line := getFuncLine(p, pc)
		src := p.Source
		if p.NoSource {
			src = "=?"
		}
		ss := shortSrc(src)
		var eb strings.Builder
		eb.WriteString("\n\t")
		eb.WriteString(ss)
		eb.WriteByte(':')
		if line > 0 {
			eb.WriteString(fmt.Sprintf("%d:", line))
		}
		if p.LineDefined == 0 {
			eb.WriteString(" in main chunk")
		} else if name, namewhat := funcNameFromCall(th, i); namewhat != "" {
			if namewhat == "metamethod" {
				eb.WriteString(fmt.Sprintf(" in metamethod '%s'", name))
			} else if namewhat == "hook" {
				eb.WriteString(" in hook '?'")
			} else if namewhat == "for iterator" {
				eb.WriteString(" in for iterator 'for iterator'")
			} else {
				eb.WriteString(fmt.Sprintf(" in %s '%s'", namewhat, name))
			}
		} else {
			eb.WriteString(fmt.Sprintf(" in function <%s:%d>", ss, p.LineDefined))
		}
		if cf.isTailCall {
			eb.WriteString("\n\t(...tail calls...)")
		}
		entries = append(entries, eb.String())
	}
	if level >= len(entries) {
		return sb.String()
	}
	shown := entries[level:]
	const (
		levels1 = 10
		levels2 = 11
	)
	if len(shown) > levels1+levels2+1 {
		for i := 0; i < levels1; i++ {
			sb.WriteString(shown[i])
		}
		skipped := len(shown) - levels1 - levels2
		sb.WriteString(fmt.Sprintf("\n\t...\t(skipping %d levels)", skipped))
		for i := len(shown) - levels2; i < len(shown); i++ {
			sb.WriteString(shown[i])
		}
	} else {
		for _, e := range shown {
			sb.WriteString(e)
		}
	}
	return sb.String()
}

func nativeDebugTraceback(s *State, args []Value) ([]Value, error) {
	th, rest := optThreadArg(s, args)
	var msg string
	hasMsg := false
	if len(rest) >= 1 && !rest[0].IsNil() {
		if !rest[0].IsString() && !rest[0].IsNumber() {
			return []Value{rest[0]}, nil // non-string error object returned verbatim per Lua spec!
		}
		msg = FormatValue(rest[0])
		hasMsg = true
	}
	level := 1
	explicitLevel := false
	if len(rest) >= 2 && !rest[1].IsNil() {
		lv, ok := rest[1].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument to 'traceback' (number expected)")
		}
		level = int(lv)
		explicitLevel = true
	}
	if th != s.cur && !explicitLevel {
		level = 0
	}
	hasNativeTop := th.nframe > 0 && th.frames[th.nframe-1].kind == frameNative
	includeSelf := th == s.cur && level == 0 && !hasNativeTop
	if th == s.cur && level > 0 && !hasNativeTop {
		level--
	}
	if hasMsg && s.memLimit > 0 {
		if err := s.checkMem(int64(len(msg)) + 256); err != nil {
			return nil, err
		}
	}
	tb := s.formatTraceback(th, msg, hasMsg, level)
	if includeSelf {
		tb = strings.Replace(tb, "stack traceback:", "stack traceback:\n\t[C]: in function 'traceback'", 1)
	}
	ls, err := s.TryIntern(tb)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

func nativeUpvalueCount(nf *NativeFunc) int {
	n := len(nf.upvals)
	if int(nf.NumUpvals) > n {
		n = int(nf.NumUpvals)
	}
	return n
}

func fillCFrameInfo(s *State, what string, cfunc Value, cname, namewhat string, ftransfer, ntransfer int64) (*Table, error) {
	info, err := s.TryNewTable(0, 10)
	if err != nil {
		return nil, err
	}
	nups := int64(0)
	if cfunc.IsNative() {
		nups = int64(nativeUpvalueCount(cfunc.AsNative()))
	}
	if strings.ContainsRune(what, 'S') {
		info.RawSetStr(s, s.intern.Intern("what"), StringValue(s.intern.Intern("C")))
		info.RawSetStr(s, s.intern.Intern("source"), StringValue(s.intern.Intern("=[C]")))
		info.RawSetStr(s, s.intern.Intern("short_src"), StringValue(s.intern.Intern("[C]")))
		info.RawSetStr(s, s.intern.Intern("linedefined"), IntValue(-1))
		info.RawSetStr(s, s.intern.Intern("lastlinedefined"), IntValue(-1))
	}
	if strings.ContainsRune(what, 'l') {
		info.RawSetStr(s, s.intern.Intern("currentline"), IntValue(-1))
	}
	if strings.ContainsRune(what, 'n') {
		info.RawSetStr(s, s.intern.Intern("name"), StringValue(s.intern.Intern(cname)))
		info.RawSetStr(s, s.intern.Intern("namewhat"), StringValue(s.intern.Intern(namewhat)))
	}
	if strings.ContainsRune(what, 'u') {
		info.RawSetStr(s, s.intern.Intern("nups"), IntValue(nups))
		info.RawSetStr(s, s.intern.Intern("nparams"), IntValue(0))
		info.RawSetStr(s, s.intern.Intern("isvararg"), TrueValue())
	}
	if strings.ContainsRune(what, 't') {
		info.RawSetStr(s, s.intern.Intern("istailcall"), FalseValue())
		info.RawSetStr(s, s.intern.Intern("extraargs"), IntValue(0))
	}
	if strings.ContainsRune(what, 'r') {
		info.RawSetStr(s, s.intern.Intern("ftransfer"), IntValue(ftransfer))
		info.RawSetStr(s, s.intern.Intern("ntransfer"), IntValue(ntransfer))
	}
	if strings.ContainsRune(what, 'f') {
		info.RawSetStr(s, s.intern.Intern("func"), cfunc)
	}
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return info, nil
}

func nativeDebugGetInfo(s *State, args []Value) ([]Value, error) {
	th, rest := optThreadArg(s, args)
	if len(rest) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'getinfo'")
	}
	what := "flnSrtu"
	if len(rest) >= 2 && !rest[1].IsNil() {
		if !rest[1].IsString() {
			return nil, fmt.Errorf("bad argument #2 to 'getinfo' (string expected)")
		}
		what = rest[1].AsString().S
		for i := 0; i < len(what); i++ {
			switch what[i] {
			case 'f', 'l', 'n', 'r', 'S', 't', 'u', 'L':
			default:
				return nil, fmt.Errorf("bad argument #2 to 'getinfo' (invalid option '%c')", what[i])
			}
		}
	}

	var fn Value
	var cf *callFrame
	cfIdx := -1
	if rest[0].IsFunction() {
		fn = rest[0]
	} else if lv, ok := rest[0].CoerceInt(); ok {
		lvl := int(lv)
		if th == s.cur && lvl == 0 {
			// Level 0 is debug.getinfo itself (C/Go function).
			cfunc := NilValue()
			if s.builtinDebugGetInfo != nil {
				cfunc = NativeValue(s.builtinDebugGetInfo)
			}
			info, err := fillCFrameInfo(s, what, cfunc, "getinfo", "field", 0, 0)
			if err != nil {
				return nil, err
			}
			return []Value{TableValue(info)}, nil
		}
		// Level 0 on a suspended coroutine is coroutine.yield (C frame).
		if th != s.cur && th.status == ThreadSuspended && lvl == 0 {
			cfunc := NilValue()
			if s.builtinCoroYield != nil {
				cfunc = NativeValue(s.builtinCoroYield)
			}
			info, err := fillCFrameInfo(s, what, cfunc, "yield", "field", 0, 0)
			if err != nil {
				return nil, err
			}
			return []Value{TableValue(info)}, nil
		}
		// Walk frames (1-based on current thread unless frameNative for getinfo itself is already on top;
		// also 1-based on suspended coroutine because level 0 is coroutine.yield).
		target := lvl
		if (th == s.cur && !(th.nframe > 0 && th.frames[th.nframe-1].kind == frameNative)) ||
			(th != s.cur && th.status == ThreadSuspended) {
			target = lvl - 1
		}
		if target < 0 {
			return []Value{NilValue()}, nil
		}
		seen := 0
		for i := th.nframe - 1; i >= 0; i-- {
			if seen == target {
				cf = &th.frames[i]
				cfIdx = i
				if cf.kind == frameLua {
					fn = ClosureValue(cf.cl)
				}
				break
			}
			seen++
		}
		if cf == nil {
			return []Value{NilValue()}, nil
		}
		if cf.kind != frameLua {
			// framePCall / frameXPCall / frameNative C boundary frame!
			cfunc := cf.nativeFn
			cname, namewhat := "", ""
			if cfIdx > 0 && th.frames[cfIdx-1].kind == frameLua {
				cname, namewhat = funcNameFromCall(th, cfIdx)
			}
			if cname == "" {
				if cf.kind == framePCall {
					cname = "pcall"
					namewhat = "global"
				} else if cf.kind == frameXPCall {
					cname = "xpcall"
					namewhat = "global"
				} else if cf.nativeFn.IsNative() {
					cname = cf.nativeFn.AsNative().Name
					namewhat = "global"
				}
			}
			if cfunc.IsNil() {
				if cf.kind == framePCall {
					cfunc = s.GetGlobal("pcall")
				} else if cf.kind == frameXPCall {
					cfunc = s.GetGlobal("xpcall")
				}
			}
			info, err := fillCFrameInfo(s, what, cfunc, cname, namewhat, int64(cf.ftransfer), int64(cf.ntransfer))
			if err != nil {
				return nil, err
			}
			return []Value{TableValue(info)}, nil
		}
	} else {
		return nil, fmt.Errorf("bad argument #1 to 'getinfo' (function or level expected)")
	}

	info, err := s.TryNewTable(0, 12)
	if err != nil {
		return nil, err
	}
	if fn.IsClosure() {
		p := fn.AsClosure().bp.proto
		if strings.ContainsRune(what, 'S') {
			src := p.Source
			if p.NoSource {
				src = "=?"
			}
			info.RawSetStr(s, s.intern.Intern("source"), StringValue(s.intern.Intern(src)))
			info.RawSetStr(s, s.intern.Intern("short_src"), StringValue(s.intern.Intern(shortSrc(src))))
			info.RawSetStr(s, s.intern.Intern("linedefined"), IntValue(int64(p.LineDefined)))
			info.RawSetStr(s, s.intern.Intern("lastlinedefined"), IntValue(int64(p.LastLineDefined)))
			w := "Lua"
			if p.LineDefined == 0 {
				w = "main"
			}
			info.RawSetStr(s, s.intern.Intern("what"), StringValue(s.intern.Intern(w)))
		}
		if strings.ContainsRune(what, 'l') {
			line := -1
			if cf != nil {
				pc := cf.pc
				if !cf.atCurrentPC && pc > 0 {
					pc--
				}
				line = getFuncLine(p, pc)
			}
			info.RawSetStr(s, s.intern.Intern("currentline"), IntValue(int64(line)))
		}
		if strings.ContainsRune(what, 'u') {
			info.RawSetStr(s, s.intern.Intern("nups"), IntValue(int64(len(p.Upvalues))))
			info.RawSetStr(s, s.intern.Intern("nparams"), IntValue(int64(p.NumParams)))
			info.RawSetStr(s, s.intern.Intern("isvararg"), BoolValue(p.IsVararg()))
		}
		if strings.ContainsRune(what, 't') {
			tc := false
			ea := int64(0)
			if cf != nil {
				tc = cf.isTailCall
				ea = int64(cf.extraArgs)
			}
			info.RawSetStr(s, s.intern.Intern("istailcall"), BoolValue(tc))
			info.RawSetStr(s, s.intern.Intern("extraargs"), IntValue(ea))
		}
		if strings.ContainsRune(what, 'n') {
			name, namewhat := "", ""
			if cf != nil && cfIdx >= 0 {
				name, namewhat = funcNameFromCall(th, cfIdx)
			}
			if name != "" {
				info.RawSetStr(s, s.intern.Intern("name"), StringValue(s.intern.Intern(name)))
			}
			info.RawSetStr(s, s.intern.Intern("namewhat"), StringValue(s.intern.Intern(namewhat)))
		}
		if strings.ContainsRune(what, 'r') {
			ft, nt := int64(0), int64(0)
			if cf != nil {
				ft = int64(cf.ftransfer)
				nt = int64(cf.ntransfer)
			}
			info.RawSetStr(s, s.intern.Intern("ftransfer"), IntValue(ft))
			info.RawSetStr(s, s.intern.Intern("ntransfer"), IntValue(nt))
		}
		if strings.ContainsRune(what, 'f') {
			info.RawSetStr(s, s.intern.Intern("func"), fn)
		}
		if strings.ContainsRune(what, 'L') {
			al, err := s.TryNewTable(0, 16)
			if err != nil {
				return nil, err
			}
			startPC := 0
			if len(p.Code) > 0 && p.Code[0].Op() == bytecode.OP_VARARGPREP {
				startPC = 1
			}
			for pc := startPC; pc < len(p.LineInfo); pc++ {
				ln := getFuncLine(p, pc)
				if ln > 0 {
					al.RawSetInt(s, int64(ln), TrueValue())
					if err := s.ConsumeMemErr(); err != nil {
						return nil, err
					}
				}
			}
			info.RawSetStr(s, s.intern.Intern("activelines"), TableValue(al))
		}
	} else {
		// Native Go function
		if strings.ContainsRune(what, 'S') {
			info.RawSetStr(s, s.intern.Intern("source"), StringValue(s.intern.Intern("=[C]")))
			info.RawSetStr(s, s.intern.Intern("short_src"), StringValue(s.intern.Intern("[C]")))
			info.RawSetStr(s, s.intern.Intern("linedefined"), IntValue(-1))
			info.RawSetStr(s, s.intern.Intern("lastlinedefined"), IntValue(-1))
			info.RawSetStr(s, s.intern.Intern("what"), StringValue(s.intern.Intern("C")))
		}
		if strings.ContainsRune(what, 'l') {
			info.RawSetStr(s, s.intern.Intern("currentline"), IntValue(-1))
		}
		if strings.ContainsRune(what, 'u') {
			nups := int64(0)
			if fn.IsNative() {
				nups = int64(nativeUpvalueCount(fn.AsNative()))
			}
			info.RawSetStr(s, s.intern.Intern("nups"), IntValue(nups))
			info.RawSetStr(s, s.intern.Intern("nparams"), IntValue(0))
			info.RawSetStr(s, s.intern.Intern("isvararg"), TrueValue())
		}
		if strings.ContainsRune(what, 't') {
			info.RawSetStr(s, s.intern.Intern("istailcall"), FalseValue())
			info.RawSetStr(s, s.intern.Intern("extraargs"), IntValue(0))
		}
		if strings.ContainsRune(what, 'f') {
			info.RawSetStr(s, s.intern.Intern("func"), fn)
		}
	}
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{TableValue(info)}, nil
}

func findLocalName(p *bytecode.Proto, localIdx, pc int) (string, bool) {
	n := localIdx
	for _, lv := range p.LocVars {
		if lv.StartPC <= pc && pc < lv.EndPC {
			n--
			if n == 0 {
				return lv.Name, true
			}
		}
	}
	return "", false
}

func opSetsA(op bytecode.OpCode) bool {
	switch op {
	case bytecode.OP_SETUPVAL, bytecode.OP_SETTABUP, bytecode.OP_SETTABLE, bytecode.OP_SETI, bytecode.OP_SETFIELD,
		bytecode.OP_MMBIN, bytecode.OP_MMBINI, bytecode.OP_MMBINK,
		bytecode.OP_CLOSE, bytecode.OP_TBC, bytecode.OP_JMP,
		bytecode.OP_EQ, bytecode.OP_LT, bytecode.OP_LE, bytecode.OP_EQK, bytecode.OP_EQI,
		bytecode.OP_LTI, bytecode.OP_LEI, bytecode.OP_GTI, bytecode.OP_GEI, bytecode.OP_TEST,
		bytecode.OP_RETURN, bytecode.OP_RETURN0, bytecode.OP_RETURN1,
		bytecode.OP_FORLOOP, bytecode.OP_FORPREP, bytecode.OP_TFORPREP, bytecode.OP_TFORCALL, bytecode.OP_TFORLOOP,
		bytecode.OP_SETLIST, bytecode.OP_ERRNNIL, bytecode.OP_VARARGPREP, bytecode.OP_EXTRAARG:
		return false
	default:
		return true
	}
}

// findObjectName mirrors Lua 5.5 ldebug.c:getobjname to deduce (name, namewhat) for register reg at pc.
func findObjectName(p *bytecode.Proto, reg, lastpc int) (string, string) {
	if name, ok := findLocalName(p, reg+1, lastpc); ok {
		return name, "local"
	}
	// Scan backwards for the last instruction that wrote to reg.
	setpc := -1
	jmptarget := 0
	for pc := 0; pc < lastpc; pc++ {
		w := p.Code[pc]
		op := w.Op()
		a := int(w.A())
		sets := false
		switch op {
		case bytecode.OP_LOADNIL:
			b := int(w.B())
			if a <= reg && reg <= a+b {
				sets = true
			}
		case bytecode.OP_TFORCALL:
			if reg >= a+2 {
				sets = true
			}
		case bytecode.OP_CALL, bytecode.OP_TAILCALL:
			if reg >= a {
				sets = true
			}
		case bytecode.OP_JMP:
			dest := pc + 1 + w.SJ()
			if dest <= lastpc && dest > jmptarget {
				jmptarget = dest
			}
		default:
			if opSetsA(op) && reg == a {
				sets = true
			}
		}
		if sets {
			if pc < jmptarget {
				setpc = -1 // ambiguous origin across control-flow branch (ldebug.c:findsetreg)
			} else {
				setpc = pc
			}
		}
	}
	if setpc < 0 {
		return "", ""
	}
	w := p.Code[setpc]
	switch w.Op() {
	case bytecode.OP_MOVE:
		b := int(w.B())
		if b < int(w.A()) {
			return findObjectName(p, b, setpc)
		}
	case bytecode.OP_GETTABUP:
		uvIdx := int(w.B())
		kIdx := int(w.C())
		if kIdx < len(p.Consts) && p.Consts[kIdx].Kind == bytecode.ConstString {
			uvName := ""
			if uvIdx < len(p.Upvalues) {
				uvName = p.Upvalues[uvIdx].Name
			}
			if uvName == "_ENV" {
				return p.Consts[kIdx].Str, "global"
			}
			return p.Consts[kIdx].Str, "field"
		}
	case bytecode.OP_GETTABLE, bytecode.OP_GETFIELD, bytecode.OP_GETI:
		if w.Op() == bytecode.OP_GETFIELD {
			kIdx := int(w.C())
			if kIdx < len(p.Consts) && p.Consts[kIdx].Kind == bytecode.ConstString {
				treg := int(w.B())
				if ln, ok := findLocalName(p, treg+1, setpc); ok && ln == "_ENV" {
					return p.Consts[kIdx].Str, "global"
				}
				return p.Consts[kIdx].Str, "field"
			}
		} else if w.Op() == bytecode.OP_GETI {
			return "integer index", "field"
		} else if w.Op() == bytecode.OP_GETTABLE {
			// When constant index > 255, Lua 5.5 emits OP_LOADK/OP_LOADKX + OP_GETTABLE (errors.lua:327-331).
			kReg := int(w.C())
			for pc := setpc - 1; pc >= 0; pc-- {
				kw := p.Code[pc]
				if (kw.Op() == bytecode.OP_LOADK || kw.Op() == bytecode.OP_LOADKX) && int(kw.A()) == kReg {
					idx := kw.Bx()
					if kw.Op() == bytecode.OP_LOADKX && pc+1 < len(p.Code) {
						idx = p.Code[pc+1].Ax()
					}
					if idx < len(p.Consts) && p.Consts[idx].Kind == bytecode.ConstString {
						treg := int(w.B())
						if ln, ok := findLocalName(p, treg+1, setpc); ok && ln == "_ENV" {
							return p.Consts[idx].Str, "global"
						}
						if tregName, tregKind := findObjectName(p, treg, setpc); tregKind == "upvalue" && tregName == "_ENV" {
							return p.Consts[idx].Str, "global"
						}
						return p.Consts[idx].Str, "field"
					}
					break
				}
				if opSetsA(kw.Op()) && int(kw.A()) == kReg {
					break
				}
			}
		}
	case bytecode.OP_GETUPVAL:
		uvIdx := int(w.B())
		if uvIdx < len(p.Upvalues) && p.Upvalues[uvIdx].Name != "" {
			return p.Upvalues[uvIdx].Name, "upvalue"
		}
	case bytecode.OP_GETVARG:
		kReg := int(w.C())
		// Look backwards for OP_LOADK loading the constant field name into kReg.
		for pc := setpc - 1; pc >= 0; pc-- {
			kw := p.Code[pc]
			if kw.Op() == bytecode.OP_LOADK && int(kw.A()) == kReg {
				idx := kw.Bx()
				if idx < len(p.Consts) && p.Consts[idx].Kind == bytecode.ConstString {
					return p.Consts[idx].Str, "field"
				}
				break
			}
			if opSetsA(kw.Op()) && int(kw.A()) == kReg {
				break
			}
		}
		return "", ""
	case bytecode.OP_LOADK:
		idx := w.Bx()
		if idx < len(p.Consts) && p.Consts[idx].Kind == bytecode.ConstString {
			return p.Consts[idx].Str, "constant"
		}
	case bytecode.OP_LOADKX:
		if setpc+1 < len(p.Code) {
			idx := p.Code[setpc+1].Ax()
			if idx < len(p.Consts) && p.Consts[idx].Kind == bytecode.ConstString {
				return p.Consts[idx].Str, "constant"
			}
		}
	case bytecode.OP_SELF:
		kIdx := int(w.C())
		if kIdx < len(p.Consts) && p.Consts[kIdx].Kind == bytecode.ConstString {
			return p.Consts[kIdx].Str, "method"
		}
	}
	return "", ""
}

// funcNameFromCall inspects the caller frame of th.frames[cfIdx] to deduce (name, namewhat).
func funcNameFromCall(th *Thread, cfIdx int) (string, string) {
	if cfIdx < 0 || cfIdx >= th.nframe {
		return "", ""
	}
	if th.frames[cfIdx].isHook {
		return "?", "hook"
	}
	if th.frames[cfIdx].isFinalizer {
		return "__gc", "metamethod"
	}
	if cfIdx == 0 || th.frames[cfIdx].isTailCall {
		return "", ""
	}
	// Only inspect immediate Lua caller frame (matching ldebug.c:funcnamefromcall;
	// callbacks invoked from C frames like table.sort/pcall/gsub have no caller name).
	callerIdx := cfIdx - 1
	if callerIdx < 0 || th.frames[callerIdx].kind != frameLua {
		return "", ""
	}
	caller := &th.frames[callerIdx]
	if caller.cl == nil {
		return "", ""
	}
	p := caller.cl.bp.proto
	pc := caller.pc - 1
	if pc < 0 || pc >= len(p.Code) {
		return "", ""
	}
	w := p.Code[pc]
	switch w.Op() {
	case bytecode.OP_CALL, bytecode.OP_TAILCALL:
		return findObjectName(p, int(w.A()), pc)
	case bytecode.OP_TFORCALL:
		return "for iterator", "for iterator"
	case bytecode.OP_SELF, bytecode.OP_GETTABUP, bytecode.OP_GETTABLE, bytecode.OP_GETI, bytecode.OP_GETFIELD:
		return "index", "metamethod"
	case bytecode.OP_SETTABUP, bytecode.OP_SETTABLE, bytecode.OP_SETI, bytecode.OP_SETFIELD:
		return "newindex", "metamethod"
	case bytecode.OP_MMBIN, bytecode.OP_MMBINI, bytecode.OP_MMBINK:
		tm := TM(w.C())
		if tm < tmCount {
			return strings.TrimPrefix(tmNames[tm], "__"), "metamethod"
		}
	case bytecode.OP_UNM:
		return "unm", "metamethod"
	case bytecode.OP_BNOT:
		return "bnot", "metamethod"
	case bytecode.OP_LEN:
		return "len", "metamethod"
	case bytecode.OP_CONCAT:
		return "concat", "metamethod"
	case bytecode.OP_EQ:
		return "eq", "metamethod"
	case bytecode.OP_LT, bytecode.OP_LTI, bytecode.OP_GTI:
		return "lt", "metamethod"
	case bytecode.OP_LE, bytecode.OP_LEI, bytecode.OP_GEI:
		return "le", "metamethod"
	case bytecode.OP_CLOSE, bytecode.OP_RETURN:
		return "close", "metamethod"
	}
	return "", ""
}

func findLuaFrame(s *State, th *Thread, level int) *callFrame {
	target := level
	if th == s.cur || th.status == ThreadSuspended {
		target = level - 1
	}
	if target < 0 {
		return nil
	}
	seen := 0
	for i := th.nframe - 1; i >= 0; i-- {
		if th.frames[i].kind != frameLua {
			continue
		}
		if seen == target {
			return &th.frames[i]
		}
		seen++
	}
	return nil
}

func frameLocalLimit(s *State, th *Thread, cfIdx int) int {
	cf := &th.frames[cfIdx]
	limit := int(cf.cl.bp.proto.MaxStackSize)
	if cfIdx+1 < th.nframe {
		if nextLim := th.frames[cfIdx+1].funcIdx - cf.base; nextLim >= 0 && nextLim < limit {
			limit = nextLim
		}
	} else if th != s.cur && th.status == ThreadSuspended {
		if yLim := th.yieldDst - cf.base; yLim >= 0 && yLim < limit {
			limit = yLim
		}
	} else if th == s.cur {
		if curLim := th.top - cf.base; curLim >= 0 && curLim < limit {
			limit = curLim
		}
	}
	if cf.ntransfer > 0 {
		if tLim := int(cf.ftransfer) + int(cf.ntransfer) - 1; tLim > limit {
			limit = tLim
		}
	}
	return limit
}

func resolveFrameIndex(s *State, th *Thread, lv int) int {
	target := lv
	if (th == s.cur && !(th.nframe > 0 && th.frames[th.nframe-1].kind == frameNative)) ||
		(th != s.cur && th.status == ThreadSuspended) {
		target--
	}
	if target < 0 {
		return -1
	}
	seen := 0
	for i := th.nframe - 1; i >= 0; i-- {
		if seen == target {
			return i
		}
		seen++
	}
	return -1
}

func nativeDebugGetLocal(s *State, args []Value) ([]Value, error) {
	th, rest := optThreadArg(s, args)
	if len(rest) < 2 {
		return nil, fmt.Errorf("bad arguments to 'getlocal'")
	}
	idx, ok := rest[1].CoerceInt()
	if !ok {
		return nil, fmt.Errorf("bad argument to 'getlocal' (number expected)")
	}
	if rest[0].IsClosure() {
		p := rest[0].AsClosure().bp.proto
		if idx < 1 || int(idx) > int(p.NumParams) {
			return []Value{NilValue()}, nil
		}
		if name, ok := findLocalName(p, int(idx), 0); ok {
			return []Value{StringValue(s.intern.Intern(name))}, nil
		}
		return []Value{NilValue()}, nil
	}
	if rest[0].IsNative() {
		return []Value{NilValue()}, nil
	}
	lv, ok := rest[0].CoerceInt()
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'getlocal' (function or level expected)")
	}
	if lv == 0 && th == s.cur {
		if idx < 1 || int(idx) > len(args) {
			return []Value{NilValue()}, nil
		}
		return []Value{StringValue(s.intern.Intern("(C temporary)")), args[idx-1]}, nil
	}
	cfIdx := resolveFrameIndex(s, th, int(lv))
	if cfIdx < 0 {
		return nil, fmt.Errorf("bad argument #1 to 'getlocal' (level out of range)")
	}
	cf := &th.frames[cfIdx]
	if cf.kind != frameLua {
		if idx < 1 || int(idx) > int(cf.ntransfer) {
			return []Value{NilValue()}, nil
		}
		return []Value{StringValue(s.intern.Intern("(C temporary)")), th.stack[cf.base+int(idx)-1]}, nil
	}
	p := cf.cl.bp.proto
	if idx < 0 {
		// Negative index accesses hidden varargs only (matching PUC-Rio ldebug.c:findvararg: PF_VAHID).
		// Guard against -math.MinInt64 overflow.
		if p.Flag&bytecode.PFVarargHidden == 0 || idx == math.MinInt64 {
			return []Value{NilValue()}, nil
		}
		vi := -idx
		if vi >= 1 && vi <= int64(cf.nextraargs) {
			return []Value{StringValue(s.intern.Intern("(vararg)")), th.stack[cf.base-cf.nextraargs+int(vi)-1]}, nil
		}
		return []Value{NilValue()}, nil
	}
	if idx == 0 {
		return []Value{NilValue()}, nil
	}
	pc := cf.pc
	if !cf.atCurrentPC && pc > 0 {
		pc--
	}
	limit := frameLocalLimit(s, th, cfIdx)
	if idx > int64(limit) {
		return []Value{NilValue()}, nil
	}
	if name, ok := findLocalName(p, int(idx), pc); ok {
		return []Value{StringValue(s.intern.Intern(name)), th.stack[cf.base+int(idx)-1]}, nil
	}
	return []Value{StringValue(s.intern.Intern("(temporary)")), th.stack[cf.base+int(idx)-1]}, nil
}

func nativeDebugSetLocal(s *State, args []Value) ([]Value, error) {
	th, rest := optThreadArg(s, args)
	if len(rest) < 3 {
		return nil, fmt.Errorf("bad arguments to 'setlocal'")
	}
	lv, ok1 := rest[0].CoerceInt()
	idx, ok2 := rest[1].CoerceInt()
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("bad arguments to 'setlocal'")
	}
	cfIdx := resolveFrameIndex(s, th, int(lv))
	if cfIdx < 0 {
		return nil, fmt.Errorf("bad argument #1 to 'setlocal' (level out of range)")
	}
	cf := &th.frames[cfIdx]
	if cf.kind != frameLua {
		if idx < 1 || idx > int64(cf.ntransfer) {
			return []Value{NilValue()}, nil
		}
		th.stack[cf.base+int(idx)-1] = rest[2]
		return []Value{StringValue(s.intern.Intern("(C temporary)"))}, nil
	}
	p := cf.cl.bp.proto
	if idx < 0 {
		// Negative index accesses hidden varargs only (matching PUC-Rio ldebug.c:findvararg: PF_VAHID).
		if p.Flag&bytecode.PFVarargHidden == 0 || idx == math.MinInt64 {
			return []Value{NilValue()}, nil
		}
		vi := -idx
		if vi >= 1 && vi <= int64(cf.nextraargs) {
			th.stack[cf.base-cf.nextraargs+int(vi)-1] = rest[2]
			return []Value{StringValue(s.intern.Intern("(vararg)"))}, nil
		}
		return []Value{NilValue()}, nil
	}
	limit := frameLocalLimit(s, th, cfIdx)
	if idx < 1 || idx > int64(limit) {
		return []Value{NilValue()}, nil
	}
	th.stack[cf.base+int(idx)-1] = rest[2]
	pc := cf.pc
	if !cf.atCurrentPC && pc > 0 {
		pc--
	}
	if name, ok := findLocalName(p, int(idx), pc); ok {
		return []Value{StringValue(s.intern.Intern(name))}, nil
	}
	return []Value{StringValue(s.intern.Intern("(temporary)"))}, nil
}

func nativeDebugGetUpvalue(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("bad arguments to 'getupvalue'")
	}
	idx, ok := args[1].CoerceInt()
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'getupvalue'")
	}
	if args[0].IsNative() {
		nf := args[0].AsNative()
		n := nativeUpvalueCount(nf)
		if idx >= 1 && int(idx) <= n {
			val := NilValue()
			if int(idx)-1 < len(nf.upvals) {
				val = nf.upvals[idx-1]
			}
			return []Value{StringValue(s.intern.Intern("")), val}, nil
		}
		return []Value{NilValue()}, nil
	}
	if !args[0].IsClosure() {
		return []Value{NilValue()}, nil
	}
	cl := args[0].AsClosure()
	if idx < 1 || int(idx) > len(cl.upvals) {
		return []Value{NilValue()}, nil
	}
	name := ""
	if int(idx)-1 < len(cl.bp.proto.Upvalues) {
		name = cl.bp.proto.Upvalues[idx-1].Name
	}
	if name == "" {
		name = "(no name)"
	}
	return []Value{StringValue(s.intern.Intern(name)), cl.upvals[idx-1].Get()}, nil
}

func nativeDebugSetUpvalue(s *State, args []Value) ([]Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("bad arguments to 'setupvalue'")
	}
	idx, ok := args[1].CoerceInt()
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'setupvalue'")
	}
	if args[0].IsNative() {
		nf := args[0].AsNative()
		n := nativeUpvalueCount(nf)
		if idx >= 1 && int(idx) <= n {
			if int(idx)-1 < len(nf.upvals) {
				nf.upvals[idx-1] = args[2]
			}
			return []Value{StringValue(s.intern.Intern(""))}, nil
		}
		return []Value{NilValue()}, nil
	}
	if !args[0].IsClosure() {
		return []Value{NilValue()}, nil
	}
	cl := args[0].AsClosure()
	if idx < 1 || int(idx) > len(cl.upvals) {
		return []Value{NilValue()}, nil
	}
	cl.upvals[idx-1].Set(args[2])
	s.protoEpoch++ // invalidate any upvalue-dependent IC
	name := ""
	if int(idx)-1 < len(cl.bp.proto.Upvalues) {
		name = cl.bp.proto.Upvalues[idx-1].Name
	}
	if name == "" {
		name = "(no name)"
	}
	return []Value{StringValue(s.intern.Intern(name))}, nil
}

func nativeDebugUpvalueID(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("bad arguments to 'upvalueid'")
	}
	idx, ok := args[1].CoerceInt()
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'upvalueid'")
	}
	if args[0].IsNative() {
		nf := args[0].AsNative()
		n := nativeUpvalueCount(nf)
		if idx < 1 || int(idx) > n {
			return []Value{NilValue()}, nil
		}
		i := int(idx) - 1
		if nf.Collectable {
			if len(nf.upvalIDs) < n {
				grown := make([]uint64, n)
				copy(grown, nf.upvalIDs)
				nf.upvalIDs = grown
			}
			if nf.upvalIDs[i] == 0 {
				nf.upvalIDs[i] = globalUpvalTokenID.Add(1)
			}
			return []Value{LightUDTokenValue(nf.upvalIDs[i])}, nil
		}
		if s.immUpvalTokens == nil {
			s.immUpvalTokens = make(map[*NativeFunc][]uint64)
		}
		ids := s.immUpvalTokens[nf]
		if len(ids) < n {
			grown := make([]uint64, n)
			copy(grown, ids)
			ids = grown
			s.immUpvalTokens[nf] = ids
		}
		if ids[i] == 0 {
			ids[i] = globalUpvalTokenID.Add(1)
		}
		return []Value{LightUDTokenValue(ids[i])}, nil
	}
	if !args[0].IsClosure() {
		return []Value{NilValue()}, nil
	}
	cl := args[0].AsClosure()
	if idx < 1 || int(idx) > len(cl.upvals) {
		return []Value{NilValue()}, nil
	}
	return []Value{LightUDTokenValue(cl.upvals[idx-1].tokenID())}, nil
}

func nativeDebugUpvalueJoin(s *State, args []Value) ([]Value, error) {
	if len(args) < 4 || !args[0].IsClosure() || !args[2].IsClosure() {
		return nil, fmt.Errorf("bad arguments to 'upvaluejoin'")
	}
	c1, c2 := args[0].AsClosure(), args[2].AsClosure()
	n1, ok1 := args[1].CoerceInt()
	n2, ok2 := args[3].CoerceInt()
	if !ok1 || !ok2 || n1 < 1 || int(n1) > len(c1.upvals) || n2 < 1 || int(n2) > len(c2.upvals) {
		return nil, fmt.Errorf("invalid upvalue index in 'upvaluejoin'")
	}
	c1.upvals[n1-1] = c2.upvals[n2-1]
	s.protoEpoch++
	return nil, nil
}

func nativeDebugGetMetatable(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'getmetatable'")
	}
	mt := s.getMetatableOf(args[0])
	if mt == nil {
		return []Value{NilValue()}, nil
	}
	return []Value{TableValue(mt)}, nil
}

func nativeDebugSetMetatable(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("bad arguments to 'setmetatable'")
	}
	var mt *Table
	if !args[1].IsNil() {
		if !args[1].IsTable() {
			return nil, fmt.Errorf("bad argument #2 to 'setmetatable' (nil or table expected)")
		}
		mt = args[1].AsTable()
	}
	s.setMetatableOf(args[0], mt)
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{args[0]}, nil
}

func nativeDebugGetRegistry(s *State, _ []Value) ([]Value, error) {
	return []Value{TableValue(s.registry)}, nil
}

func nativeDebugGetUserValue(_ *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsUserdata() {
		return []Value{NilValue(), FalseValue()}, nil
	}
	u := args[0].AsUserdata()
	n := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		n, _ = args[1].CoerceInt()
	}
	if n < 1 || int(n) > len(u.UValues) {
		return []Value{NilValue(), FalseValue()}, nil
	}
	return []Value{u.UValues[n-1], TrueValue()}, nil
}

func nativeDebugSetUserValue(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 || !args[0].IsUserdata() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'setuservalue' (userdata expected, got %s)", got)
	}
	u := args[0].AsUserdata()
	n := int64(1)
	if len(args) >= 3 && !args[2].IsNil() {
		n, _ = args[2].CoerceInt()
	}
	if n < 1 || int(n) > len(u.UValues) {
		return []Value{NilValue()}, nil
	}
	u.UValues[n-1] = args[1]
	return []Value{args[0]}, nil
}

func nativeDebugSetHook(s *State, args []Value) ([]Value, error) {
	th, rest := optThreadArg(s, args)
	if len(rest) == 0 || rest[0].IsNil() {
		th.hook = NilValue()
		th.hookMask = 0
		th.baseHookCount = 0
		th.hookCount = 0
		return nil, nil
	}
	if !rest[0].IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'sethook' (function expected, got %s)", s.objTypeName(rest[0]))
	}
	maskStr := ""
	if len(rest) >= 2 && !rest[1].IsNil() {
		if !rest[1].IsString() && !rest[1].IsNumber() {
			return nil, fmt.Errorf("bad argument #2 to 'sethook' (string expected, got %s)", s.objTypeName(rest[1]))
		}
		maskStr = FormatValue(rest[1])
	}
	var mask uint8
	for i := range len(maskStr) {
		switch maskStr[i] {
		case 'c':
			mask |= HookMaskCall
		case 'r':
			mask |= HookMaskRet
		case 'l':
			mask |= HookMaskLine
		}
	}
	count := 0
	if len(rest) >= 3 && !rest[2].IsNil() {
		c, ok := rest[2].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #3 to 'sethook' (number expected, got %s)", s.objTypeName(rest[2]))
		}
		if c > 0 {
			count = int(c)
			mask |= HookMaskCount
		}
	}
	if mask == 0 {
		th.hook = NilValue()
		th.hookMask = 0
		th.baseHookCount = 0
		th.hookCount = 0
		return nil, nil
	}
	th.hook = rest[0]
	th.hookMask = mask
	th.baseHookCount = count
	th.hookCount = count
	idx := th.nframe - 1
	if th == s.cur && idx >= 0 && th.frames[idx].kind == frameNative {
		idx--
	}
	if idx >= 0 {
		cf := &th.frames[idx]
		if cf.kind == frameLua && cf.pc > 0 {
			cf.dirty = true
			cf.oldPC = cf.pc - 1
		}
	}
	return nil, nil
}

func nativeDebugGetHook(s *State, args []Value) ([]Value, error) {
	th, _ := optThreadArg(s, args)
	if th.hook.IsNil() && th.hookMask == 0 {
		return []Value{NilValue()}, nil
	}
	var sb strings.Builder
	if th.hookMask&HookMaskCall != 0 {
		sb.WriteByte('c')
	}
	if th.hookMask&HookMaskRet != 0 {
		sb.WriteByte('r')
	}
	if th.hookMask&HookMaskLine != 0 {
		sb.WriteByte('l')
	}
	return []Value{th.hook, StringValue(s.intern.Intern(sb.String())), IntValue(int64(th.baseHookCount))}, nil
}

// nativeDebugDebug implements Lua 5.5 debug.debug() interactive prompt (main.lua:284).
func nativeDebugDebug(s *State, _ []Value) ([]Value, error) {
	sc := bufio.NewScanner(os.Stdin)
	for {
		_, _ = fmt.Fprint(s.errOut, "lua_debug> ")
		if !sc.Scan() {
			return nil, nil
		}
		line := sc.Text()
		if line == "cont" {
			return nil, nil
		}
		fn, err := s.LoadChunk([]byte(line), "=(debug command)")
		if err != nil {
			_, _ = fmt.Fprintf(s.errOut, "%s\n", s.FormatError(err))
			continue
		}
		if _, err := s.Call(fn); err != nil {
			_, _ = fmt.Fprintf(s.errOut, "%s\n", s.FormatError(err))
		}
	}
}
