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
	maxLen := bufLen - len(`[string "..."]`) - 1
	probe := source
	if len(probe) > maxLen+1 {
		probe = probe[:maxLen+1]
	}
	nl := strings.IndexByte(probe, '\n')
	truncated := nl >= 0 || len(source) > maxLen
	line := probe
	if nl >= 0 {
		line = probe[:nl]
	}
	if len(line) > maxLen {
		line = line[:maxLen]
		truncated = true
	}
	if truncated {
		return fmt.Sprintf(`[string "%s..."]`, line)
	}
	return fmt.Sprintf(`[string "%s"]`, line)
}

func (s *State) initDebugLib() *Table {
	dbg := s.newLibTable(0, 16)
	if dbg == nil {
		return nil
	}
	pinDbg := s.pinRuntime(TableValue(dbg))
	defer s.unpinRuntime(pinDbg)

	getInfo := s.registerFunc(dbg, "getinfo", intrNone, nativeDebugGetInfo)
	traceback := s.registerFunc(dbg, "traceback", intrNone, nativeDebugTraceback)
	getLocal := s.registerFunc(dbg, "getlocal", intrNone, nativeDebugGetLocal)
	setLocal := s.registerFunc(dbg, "setlocal", intrNone, nativeDebugSetLocal)
	s.registerFunc(dbg, "getupvalue", intrNone, nativeDebugGetUpvalue)
	s.registerFunc(dbg, "setupvalue", intrNone, nativeDebugSetUpvalue)
	s.registerFunc(dbg, "upvalueid", intrNone, nativeDebugUpvalueID)
	s.registerFunc(dbg, "upvaluejoin", intrNone, nativeDebugUpvalueJoin)
	s.registerFunc(dbg, "getmetatable", intrNone, nativeDebugGetMetatable)
	s.registerFunc(dbg, "setmetatable", intrNone, nativeDebugSetMetatable)
	s.registerFunc(dbg, "getregistry", intrNone, nativeDebugGetRegistry)
	s.registerFunc(dbg, "getuservalue", intrNone, nativeDebugGetUserValue)
	s.registerFunc(dbg, "setuservalue", intrNone, nativeDebugSetUserValue)
	setHook := s.registerFunc(dbg, "sethook", intrNone, nativeDebugSetHook)
	s.registerFunc(dbg, "gethook", intrNone, nativeDebugGetHook)
	s.registerFunc(dbg, "debug", intrNone, nativeDebugDebug)
	if s.memExceeded {
		return nil
	}

	// Registry _HOOKKEY weak-key table matching Lua 5.5 ldblib.c (db.lua:328).
	if s.memLimit > 0 {
		if err := s.checkMem(256); err != nil {
			s.memExceeded = true
			return nil
		}
	}
	hookKey := s.NewDictTable(4)
	pinHK := s.pinRuntime(TableValue(hookKey))
	defer s.unpinRuntime(pinHK)
	hkMeta := s.newLibTable(0, 1)
	if hkMeta == nil {
		return nil
	}
	pinHKM := s.pinRuntime(TableValue(hkMeta))
	defer s.unpinRuntime(pinHKM)
	hkMeta.RawSetStr(s, s.intern.tms[TMMode], StringValue(s.intern.Intern("k")))
	if s.memExceeded {
		return nil
	}
	s.SetMetatable(hookKey, hkMeta)
	s.registry().RawSetStr(s, s.intern.Intern("_HOOKKEY"), TableValue(hookKey))
	if s.memExceeded {
		return nil
	}
	s.builtinDebugGetInfo = getInfo
	s.builtinDebugTraceback = traceback
	s.builtinDebugGetLocal = getLocal
	s.builtinDebugSetLocal = setLocal
	s.builtinDebugSetHook = setHook
	return dbg
}

func (s *State) topFrameIsNative(th *Thread, fn *NativeFunc) bool {
	if fn == nil || th == nil || th.nframe <= 0 {
		return false
	}
	top := &th.framesSlice(s.arena)[th.nframe-1]
	return top.kind == frameNative && top.nativeFn.IsNative() && s.asNative(top.nativeFn) == fn
}

func optThreadArg(s *State, args []Value) (*Thread, []Value) {
	if len(args) > 0 && args[0].IsThread() {
		return s.asThread(args[0]), args[1:]
	}
	return s.cur(), args
}

// Traceback returns a Lua 5.5 formatted stack traceback for th (or the active thread
// when th is nil), matching PUC-Rio luaL_traceback(L, L1, msg, level).
func (s *State) Traceback(th *Thread, msg string, level int) string {
	if th == nil {
		th = s.cur()
	}
	if th == nil {
		return ""
	}
	return s.formatTraceback(th, msg, msg != "", level)
}

func (s *State) formatTraceback(th *Thread, msg string, hasMsg bool, level int) string {
	tb, _ := s.formatTracebackWithCancel(th, msg, hasMsg, level, "", nil)
	return tb
}

func (s *State) formatTracebackWithCancel(th *Thread, msg string, hasMsg bool, level int, selfEntry string, cancelFn func() error) (string, error) {
	var sb strings.Builder
	if hasMsg {
		work := 0
		if err := writeStringWithCancel(&sb, msg, cancelFn, &work); err != nil {
			return "", err
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("stack traceback:")
	if selfEntry != "" {
		sb.WriteString(selfEntry)
	}
	if level < 0 {
		return sb.String(), nil
	}
	frames := th.framesSlice(s.arena)
	var entries []string
	if th != s.cur() {
		if th.status == ThreadSuspended {
			name, namewhat := "yield", "field"
			if th.nframe > 0 && frames[th.nframe-1].kind == frameLua {
				if n, nw := s.funcNameFromCallerFrame(&frames[th.nframe-1]); nw != "" {
					name, namewhat = n, nw
				}
			}
			entries = append(entries, fmt.Sprintf("\n\t[C]: in %s '%s'", namewhat, name))
		} else if th.status == ThreadDead && !th.errObj.IsNil() && th.lastErrNative.IsNative() {
			name, namewhat := s.asNative(th.lastErrNative).Name, "function"
			if th.nframe > 0 && frames[th.nframe-1].kind == frameLua {
				if n, nw := s.funcNameFromCallerFrame(&frames[th.nframe-1]); nw != "" {
					name, namewhat = n, nw
				}
			}
			entries = append(entries, fmt.Sprintf("\n\t[C]: in %s '%s'", namewhat, name))
		}
	}
	for i := th.nframe - 1; i >= 0; i-- {
		cf := &frames[i]
		if cf.kind != frameLua {
			if cf.topProtected {
				entries = append(entries, "\n\t[C]: in ?")
				continue
			}
			if name, namewhat := s.funcNameFromCall(th, i); namewhat != "" {
				if namewhat == "metamethod" {
					entries = append(entries, fmt.Sprintf("\n\t[C]: in metamethod '%s'", name))
				} else if namewhat == "hook" {
					entries = append(entries, "\n\t[C]: in hook '?'")
				} else if namewhat == "for iterator" {
					entries = append(entries, "\n\t[C]: in for iterator 'for iterator'")
				} else {
					entries = append(entries, fmt.Sprintf("\n\t[C]: in %s '%s'", namewhat, name))
				}
				continue
			}
			cname := ""
			if cf.kind == framePCall {
				cname = "pcall"
			} else if cf.kind == frameXPCall {
				cname = "xpcall"
			} else if cf.kind == frameNative && cf.nativeFn.IsNative() {
				cname = s.asNative(cf.nativeFn).Name
			}
			if cname != "" {
				entries = append(entries, fmt.Sprintf("\n\t[C]: in function '%s'", cname))
			} else {
				entries = append(entries, "\n\t[C]: in ?")
			}
			continue
		}
		if s.frameClosure(cf) == nil {
			continue
		}
		p := s.boundProtoOf(s.frameClosure(cf)).proto
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
		} else if name, namewhat := s.funcNameFromCall(th, i); namewhat != "" {
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
		return sb.String(), nil
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
	return sb.String(), nil
}

func nativeDebugTraceback(s *State, args []Value) ([]Value, error) {
	th, rest := optThreadArg(s, args)
	var msg string
	hasMsg := false
	if len(rest) >= 1 && !rest[0].IsNil() {
		if !rest[0].IsString() && !rest[0].IsNumber() {
			return []Value{rest[0]}, nil
		}
		msg = s.FormatValue(rest[0])
		hasMsg = true
	}
	level := 1
	explicitLevel := false
	if len(rest) >= 2 && !rest[1].IsNil() {
		lv, ok, err := s.coerceInt(rest[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument to 'traceback' (number expected)")
		}
		level = int(lv)
		explicitLevel = true
	}
	if th != s.cur() && !explicitLevel {
		level = 0
	}
	hasNativeTop := s.topFrameIsNative(th, s.builtinDebugTraceback)
	includeSelf := th == s.cur() && level == 0 && !hasNativeTop
	if th == s.cur() && level > 0 && !hasNativeTop {
		level--
	}
	if hasMsg && s.memLimit > 0 {
		if err := s.checkMem(int64(len(msg)) + 256); err != nil {
			return nil, err
		}
	}
	selfEntry := ""
	if includeSelf {
		selfEntry = "\n\t[C]: in field 'traceback'"
		frames := th.framesSlice(s.arena)
		if th.nframe > 0 && frames[th.nframe-1].kind == frameLua {
			if n, nw := s.funcNameFromCallerFrame(&frames[th.nframe-1]); nw != "" {
				selfEntry = fmt.Sprintf("\n\t[C]: in %s '%s'", nw, n)
			}
		}
	}
	tb, err := s.formatTracebackWithCancel(th, msg, hasMsg, level, selfEntry, s.nativeCancelFunc())
	if err != nil {
		return nil, err
	}
	ls, err := s.TryIntern(tb)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

func (s *State) nativeUpvalueCountVal(fn Value) int {
	return s.nativeUpvalCount(fn)
}

const (
	infoFlagS uint8 = 1 << iota
	infoFlagL
	infoFlagN
	infoFlagU
	infoFlagT
	infoFlagR
	infoFlagF
	infoFlagActL
)

const defaultGetInfoFlags = infoFlagF | infoFlagL | infoFlagN | infoFlagS | infoFlagR | infoFlagT | infoFlagU

func parseGetInfoWhat(what string, cancelFn func() error) (uint8, error) {
	if len(what) > 0 && what[0] == '>' {
		return 0, fmt.Errorf("bad argument #2 to 'getinfo' (invalid option '>')")
	}
	var flags uint8
	for i := 0; i < len(what); i++ {
		if cancelFn != nil && i > 0 && (i&4095) == 0 {
			if err := cancelFn(); err != nil {
				return 0, err
			}
		}
		switch what[i] {
		case 'S':
			flags |= infoFlagS
		case 'l':
			flags |= infoFlagL
		case 'n':
			flags |= infoFlagN
		case 'u':
			flags |= infoFlagU
		case 't':
			flags |= infoFlagT
		case 'r':
			flags |= infoFlagR
		case 'f':
			flags |= infoFlagF
		case 'L':
			flags |= infoFlagActL
		default:
			return 0, fmt.Errorf("bad argument #2 to 'getinfo' (invalid option)")
		}
	}
	return flags, nil
}

func fillCFrameInfo(s *State, flags uint8, cfunc Value, cname, namewhat string, ftransfer, ntransfer int64) (*Table, error) {
	info, err := s.TryNewTable(0, 10)
	if err != nil {
		return nil, err
	}
	if s.memLimit > 0 {
		ref := s.Ref(TableValue(info))
		defer s.Unref(ref)
	}
	nups := int64(s.nativeUpvalueCountVal(cfunc))
	if flags&infoFlagS != 0 {
		info.RawSetStr(s, s.intern.Intern("what"), StringValue(s.intern.Intern("C")))
		info.RawSetStr(s, s.intern.Intern("source"), StringValue(s.intern.Intern("=[C]")))
		info.RawSetStr(s, s.intern.Intern("short_src"), StringValue(s.intern.Intern("[C]")))
		info.RawSetStr(s, s.intern.Intern("linedefined"), IntValue(-1))
		info.RawSetStr(s, s.intern.Intern("lastlinedefined"), IntValue(-1))
	}
	if flags&infoFlagL != 0 {
		info.RawSetStr(s, s.intern.Intern("currentline"), IntValue(-1))
	}
	if flags&infoFlagN != 0 {
		if cname != "" {
			info.RawSetStr(s, s.intern.Intern("name"), StringValue(s.intern.Intern(cname)))
		}
		info.RawSetStr(s, s.intern.Intern("namewhat"), StringValue(s.intern.Intern(namewhat)))
	}
	if flags&infoFlagU != 0 {
		info.RawSetStr(s, s.intern.Intern("nups"), IntValue(nups))
		info.RawSetStr(s, s.intern.Intern("nparams"), IntValue(0))
		info.RawSetStr(s, s.intern.Intern("isvararg"), TrueValue())
	}
	if flags&infoFlagT != 0 {
		info.RawSetStr(s, s.intern.Intern("istailcall"), FalseValue())
		info.RawSetStr(s, s.intern.Intern("extraargs"), IntValue(0))
	}
	if flags&infoFlagR != 0 {
		info.RawSetStr(s, s.intern.Intern("ftransfer"), IntValue(ftransfer))
		info.RawSetStr(s, s.intern.Intern("ntransfer"), IntValue(ntransfer))
	}
	if flags&infoFlagF != 0 {
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
	flags := defaultGetInfoFlags
	if len(rest) >= 2 && !rest[1].IsNil() {
		if !rest[1].IsString() {
			return nil, fmt.Errorf("bad argument #2 to 'getinfo' (string expected)")
		}
		var err error
		flags, err = parseGetInfoWhat(s.asString(rest[1]).Str(), s.nativeCancelFunc())
		if err != nil {
			return nil, err
		}
	}

	var fn Value
	var cf *callFrame
	cfIdx := -1
	if rest[0].IsFunction() {
		fn = rest[0]
	} else {
		lv, ok, err := s.coerceInt(rest[0])
		if err != nil {
			return nil, err
		}
		if ok {
			lvl := int(lv)
			if th == s.cur() && lvl == 0 {
				cfunc := NilValue()
				if s.builtinDebugGetInfo != nil {
					cfunc = s.NativeValue(s.builtinDebugGetInfo)
				}
				info, err := fillCFrameInfo(s, flags, cfunc, "getinfo", "field", 0, 0)
				if err != nil {
					return nil, err
				}
				return []Value{TableValue(info)}, nil
			}
			if th != s.cur() && th.status == ThreadSuspended && lvl == 0 {
				cfunc := NilValue()
				if s.builtinCoroYield != nil {
					cfunc = s.NativeValue(s.builtinCoroYield)
				}
				info, err := fillCFrameInfo(s, flags, cfunc, "yield", "field", 0, 0)
				if err != nil {
					return nil, err
				}
				return []Value{TableValue(info)}, nil
			}
			target := lvl
			if (th == s.cur() && !s.topFrameIsNative(th, s.builtinDebugGetInfo)) ||
				(th != s.cur() && th.status == ThreadSuspended) {
				target = lvl - 1
			}
			if target < 0 {
				return []Value{NilValue()}, nil
			}
			frames := th.framesSlice(s.arena)
			seen := 0
			for i := th.nframe - 1; i >= 0; i-- {
				if seen == target {
					cf = &frames[i]
					cfIdx = i
					if cf.kind == frameLua {
						if cl := s.frameClosure(cf); cl != nil {
							fn = ClosureValue(cl)
						}
					}
					break
				}
				seen++
			}
			if cf == nil {
				return []Value{NilValue()}, nil
			}
			if cf.kind != frameLua {
				cfunc := cf.nativeFn
				cname, namewhat := "", ""
				if !cf.topProtected && cfIdx > 0 && frames[cfIdx-1].kind == frameLua {
					cname, namewhat = s.funcNameFromCall(th, cfIdx)
				}
				if cname == "" && !cf.topProtected {
					if cf.kind == framePCall {
						cname = "pcall"
						namewhat = "global"
					} else if cf.kind == frameXPCall {
						cname = "xpcall"
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
				info, err := fillCFrameInfo(s, flags, cfunc, cname, namewhat, int64(cf.ftransfer), int64(cf.ntransfer))
				if err != nil {
					return nil, err
				}
				return []Value{TableValue(info)}, nil
			}
		} else {
			return nil, fmt.Errorf("bad argument #1 to 'getinfo' (function or level expected)")
		}
	}

	info, err := s.TryNewTable(0, 12)
	if err != nil {
		return nil, err
	}
	if s.memLimit > 0 {
		infoRef := s.Ref(TableValue(info))
		defer s.Unref(infoRef)
	}
	if fn.IsClosure() {
		p := s.boundProtoOf(s.asClosure(fn)).proto
		if flags&infoFlagS != 0 {
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
		if flags&infoFlagL != 0 {
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
		if flags&infoFlagU != 0 {
			info.RawSetStr(s, s.intern.Intern("nups"), IntValue(int64(len(p.Upvalues))))
			info.RawSetStr(s, s.intern.Intern("nparams"), IntValue(int64(p.NumParams)))
			info.RawSetStr(s, s.intern.Intern("isvararg"), BoolValue(p.IsVararg()))
		}
		if flags&infoFlagT != 0 {
			tc := false
			ea := int64(0)
			if cf != nil {
				tc = cf.isTailCall
				ea = int64(cf.extraArgs)
			}
			info.RawSetStr(s, s.intern.Intern("istailcall"), BoolValue(tc))
			info.RawSetStr(s, s.intern.Intern("extraargs"), IntValue(ea))
		}
		if flags&infoFlagN != 0 {
			name, namewhat := "", ""
			if cf != nil && cfIdx >= 0 {
				name, namewhat = s.funcNameFromCall(th, cfIdx)
			}
			if name != "" {
				info.RawSetStr(s, s.intern.Intern("name"), StringValue(s.intern.Intern(name)))
			}
			info.RawSetStr(s, s.intern.Intern("namewhat"), StringValue(s.intern.Intern(namewhat)))
		}
		if flags&infoFlagR != 0 {
			ft, nt := int64(0), int64(0)
			if cf != nil {
				ft = int64(cf.ftransfer)
				nt = int64(cf.ntransfer)
			}
			info.RawSetStr(s, s.intern.Intern("ftransfer"), IntValue(ft))
			info.RawSetStr(s, s.intern.Intern("ntransfer"), IntValue(nt))
		}
		if flags&infoFlagF != 0 {
			info.RawSetStr(s, s.intern.Intern("func"), fn)
		}
		if flags&infoFlagActL != 0 {
			al, err := s.TryNewTable(0, 16)
			if err != nil {
				return nil, err
			}
			info.RawSetStr(s, s.intern.Intern("activelines"), TableValue(al))
			if err := s.ConsumeMemErr(); err != nil {
				return nil, err
			}
			startPC := 0
			if len(p.Code) > 0 && p.Code[0].Op() == bytecode.OP_VARARGPREP {
				startPC = 1
			}
			cancelFn := s.nativeCancelFunc()
			for pc := startPC; pc < len(p.LineInfo); pc++ {
				if cancelFn != nil && pc > startPC && ((pc-startPC)&1023) == 0 {
					if cerr := cancelFn(); cerr != nil {
						return nil, cerr
					}
				}
				ln := getFuncLine(p, pc)
				if ln > 0 {
					al.RawSetInt(s, int64(ln), TrueValue())
					if err := s.ConsumeMemErr(); err != nil {
						return nil, err
					}
				}
			}
		}
	} else {
		if flags&infoFlagS != 0 {
			info.RawSetStr(s, s.intern.Intern("source"), StringValue(s.intern.Intern("=[C]")))
			info.RawSetStr(s, s.intern.Intern("short_src"), StringValue(s.intern.Intern("[C]")))
			info.RawSetStr(s, s.intern.Intern("linedefined"), IntValue(-1))
			info.RawSetStr(s, s.intern.Intern("lastlinedefined"), IntValue(-1))
			info.RawSetStr(s, s.intern.Intern("what"), StringValue(s.intern.Intern("C")))
		}
		if flags&infoFlagL != 0 {
			info.RawSetStr(s, s.intern.Intern("currentline"), IntValue(-1))
		}
		if flags&infoFlagU != 0 {
			nups := int64(s.nativeUpvalueCountVal(fn))
			info.RawSetStr(s, s.intern.Intern("nups"), IntValue(nups))
			info.RawSetStr(s, s.intern.Intern("nparams"), IntValue(0))
			info.RawSetStr(s, s.intern.Intern("isvararg"), TrueValue())
		}
		if flags&infoFlagT != 0 {
			info.RawSetStr(s, s.intern.Intern("istailcall"), FalseValue())
			info.RawSetStr(s, s.intern.Intern("extraargs"), IntValue(0))
		}
		if flags&infoFlagF != 0 {
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

func findObjectName(p *bytecode.Proto, reg, lastpc int) (string, string) {
	if name, ok := findLocalName(p, reg+1, lastpc); ok {
		return name, "local"
	}
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
				setpc = -1
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

func (s *State) funcNameFromCallerFrame(caller *callFrame) (string, string) {
	if caller == nil || caller.kind != frameLua {
		return "", ""
	}
	cl := s.frameClosure(caller)
	if cl == nil {
		return "", ""
	}
	p := s.boundProtoOf(cl).proto
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

func (s *State) funcNameFromCall(th *Thread, cfIdx int) (string, string) {
	if cfIdx < 0 || cfIdx >= th.nframe {
		return "", ""
	}
	frames := th.framesSlice(s.arena)
	if frames[cfIdx].isHook {
		return "?", "hook"
	}
	if frames[cfIdx].isFinalizer {
		return "__gc", "metamethod"
	}
	if cfIdx == 0 || frames[cfIdx].isTailCall {
		return "", ""
	}
	callerIdx := cfIdx - 1
	if callerIdx < 0 || frames[callerIdx].kind != frameLua {
		return "", ""
	}
	return s.funcNameFromCallerFrame(&frames[callerIdx])
}

func frameLocalLimit(s *State, th *Thread, cfIdx int) int {
	frames := th.framesSlice(s.arena)
	cf := &frames[cfIdx]
	limit := int(s.boundProtoOf(s.frameClosure(cf)).maxStack)
	if cfIdx+1 < th.nframe {
		if nextLim := frames[cfIdx+1].funcIdx - cf.base; nextLim >= 0 && nextLim < limit {
			limit = nextLim
		}
	} else if th != s.cur() && th.status == ThreadSuspended {
		if yLim := th.yieldDst - cf.base; yLim >= 0 && yLim < limit {
			limit = yLim
		}
	} else if th == s.cur() {
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

func resolveFrameIndex(s *State, th *Thread, lv int, self *NativeFunc) int {
	target := lv
	if (th == s.cur() && !s.topFrameIsNative(th, self)) ||
		(th != s.cur() && th.status == ThreadSuspended) {
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
	idx, ok, err := s.coerceInt(rest[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument to 'getlocal' (number expected)")
	}
	if rest[0].IsClosure() {
		p := s.boundProtoOf(s.asClosure(rest[0])).proto
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
	lv, ok, err := s.coerceInt(rest[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'getlocal' (function or level expected)")
	}
	if lv == 0 && th == s.cur() {
		if idx < 1 || int(idx) > len(args) {
			return []Value{NilValue()}, nil
		}
		return []Value{StringValue(s.intern.Intern("(C temporary)")), args[idx-1]}, nil
	}
	cfIdx := resolveFrameIndex(s, th, int(lv), s.builtinDebugGetLocal)
	if cfIdx < 0 {
		return nil, fmt.Errorf("bad argument #1 to 'getlocal' (level out of range)")
	}
	cf := &th.framesSlice(s.arena)[cfIdx]
	stack := th.stackSlice(s.arena)
	if cf.kind != frameLua {
		maxSlot := int(cf.ntransfer)
		if end := int(cf.ftransfer) + int(cf.ntransfer) - 1; end > maxSlot {
			maxSlot = end
		}
		if idx < 1 || int(idx) > maxSlot || cf.base+int(idx)-1 >= len(stack) {
			return []Value{NilValue()}, nil
		}
		return []Value{StringValue(s.intern.Intern("(C temporary)")), stack[cf.base+int(idx)-1]}, nil
	}
	p := s.boundProtoOf(s.frameClosure(cf)).proto
	if idx < 0 {
		if p.Flag&bytecode.PFVarargHidden == 0 || idx == math.MinInt64 {
			return []Value{NilValue()}, nil
		}
		vi := -idx
		if vi >= 1 && vi <= int64(cf.nextraargs) {
			return []Value{StringValue(s.intern.Intern("(vararg)")), stack[cf.base-cf.nextraargs+int(vi)-1]}, nil
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
		return []Value{StringValue(s.intern.Intern(name)), th.localSlotValue(s, cf.base+int(idx)-1)}, nil
	}
	return []Value{StringValue(s.intern.Intern("(temporary)")), th.localSlotValue(s, cf.base+int(idx)-1)}, nil
}

func nativeDebugSetLocal(s *State, args []Value) ([]Value, error) {
	th, rest := optThreadArg(s, args)
	if len(rest) < 3 {
		return nil, fmt.Errorf("bad arguments to 'setlocal'")
	}
	lv, ok1, err := s.coerceInt(rest[0])
	if err != nil {
		return nil, err
	}
	idx, ok2, err := s.coerceInt(rest[1])
	if err != nil {
		return nil, err
	}
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("bad arguments to 'setlocal'")
	}
	cfIdx := resolveFrameIndex(s, th, int(lv), s.builtinDebugSetLocal)
	if cfIdx < 0 {
		return nil, fmt.Errorf("bad argument #1 to 'setlocal' (level out of range)")
	}
	cf := &th.framesSlice(s.arena)[cfIdx]
	stack := th.stackSlice(s.arena)
	if cf.kind != frameLua {
		maxSlot := int64(cf.ntransfer)
		if end := int64(cf.ftransfer) + int64(cf.ntransfer) - 1; end > maxSlot {
			maxSlot = end
		}
		if idx < 1 || idx > maxSlot || cf.base+int(idx)-1 >= len(stack) {
			return []Value{NilValue()}, nil
		}
		stack[cf.base+int(idx)-1] = rest[2]
		return []Value{StringValue(s.intern.Intern("(C temporary)"))}, nil
	}
	p := s.boundProtoOf(s.frameClosure(cf)).proto
	if idx < 0 {
		if p.Flag&bytecode.PFVarargHidden == 0 || idx == math.MinInt64 {
			return []Value{NilValue()}, nil
		}
		vi := -idx
		if vi >= 1 && vi <= int64(cf.nextraargs) {
			stack[cf.base-cf.nextraargs+int(vi)-1] = rest[2]
			return []Value{StringValue(s.intern.Intern("(vararg)"))}, nil
		}
		return []Value{NilValue()}, nil
	}
	limit := frameLocalLimit(s, th, cfIdx)
	if idx < 1 || idx > int64(limit) {
		return []Value{NilValue()}, nil
	}
	th.setLocalSlotValue(s, cf.base+int(idx)-1, rest[2])
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
		return nil, fmt.Errorf("bad argument #2 to 'getupvalue' (number expected, got no value)")
	}
	idx, ok, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'getupvalue' (number expected, got %s)", s.objTypeName(args[1]))
	}
	if !args[0].IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'getupvalue' (function expected, got %s)", s.objTypeName(args[0]))
	}
	if args[0].IsNative() {
		n := s.nativeUpvalueCountVal(args[0])
		if idx >= 1 && int(idx) <= n {
			val, _ := s.nativeUpvalGet(args[0], int(idx)-1)
			return []Value{StringValue(s.intern.Intern("")), val}, nil
		}
		return nil, nil
	}
	cl := s.asClosure(args[0])
	if idx < 1 || int(idx) > s.closureUpvalCount(cl) {
		return nil, nil
	}
	p := s.boundProtoOf(cl).proto
	name := ""
	if int(idx)-1 < len(p.Upvalues) {
		name = p.Upvalues[idx-1].Name
	}
	if name == "" {
		name = "(no name)"
	}
	uv := s.closureUpval(cl, int(idx)-1)
	return []Value{StringValue(s.intern.Intern(name)), uv.Get(s)}, nil
}

func nativeDebugSetUpvalue(s *State, args []Value) ([]Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("bad argument #3 to 'setupvalue' (value expected)")
	}
	idx, ok, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'setupvalue' (number expected, got %s)", s.objTypeName(args[1]))
	}
	if !args[0].IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'setupvalue' (function expected, got %s)", s.objTypeName(args[0]))
	}
	if args[0].IsNative() {
		n := s.nativeUpvalueCountVal(args[0])
		if idx >= 1 && int(idx) <= n {
			s.nativeUpvalSet(args[0], int(idx)-1, args[2])
			return []Value{StringValue(s.intern.Intern(""))}, nil
		}
		return nil, nil
	}
	cl := s.asClosure(args[0])
	if idx < 1 || int(idx) > s.closureUpvalCount(cl) {
		return nil, nil
	}
	uv := s.closureUpval(cl, int(idx)-1)
	uv.Set(s, args[2])
	s.protoEpoch++
	p := s.boundProtoOf(cl).proto
	name := ""
	if int(idx)-1 < len(p.Upvalues) {
		name = p.Upvalues[idx-1].Name
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
	idx, ok, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'upvalueid'")
	}
	if args[0].IsNative() {
		n := s.nativeUpvalueCountVal(args[0])
		if idx < 1 || int(idx) > n {
			return []Value{NilValue()}, nil
		}
		return []Value{LightUDTokenValue(s.nativeUpvalID(args[0], int(idx)-1))}, nil
	}
	if !args[0].IsClosure() {
		return []Value{NilValue()}, nil
	}
	cl := s.asClosure(args[0])
	if idx < 1 || int(idx) > s.closureUpvalCount(cl) {
		return []Value{NilValue()}, nil
	}
	return []Value{LightUDTokenValue(s.closureUpval(cl, int(idx)-1).tokenID())}, nil
}

func nativeDebugUpvalueJoin(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'upvaluejoin' (number expected, got no value)")
	}
	n1, ok1, err := s.coerceInt(args[1])
	if err != nil {
		return nil, err
	}
	if !ok1 {
		return nil, fmt.Errorf("bad argument #2 to 'upvaluejoin' (number expected, got %s)", s.objTypeName(args[1]))
	}
	if !args[0].IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'upvaluejoin' (function expected, got %s)", s.objTypeName(args[0]))
	}
	if !args[0].IsClosure() {
		if n1 < 1 || int(n1) > s.nativeUpvalueCountVal(args[0]) {
			return nil, fmt.Errorf("bad argument #2 to 'upvaluejoin' (invalid upvalue index)")
		}
	} else if n1 < 1 || int(n1) > s.closureUpvalCount(s.asClosure(args[0])) {
		return nil, fmt.Errorf("bad argument #2 to 'upvaluejoin' (invalid upvalue index)")
	}
	if len(args) < 4 {
		return nil, fmt.Errorf("bad argument #4 to 'upvaluejoin' (number expected, got no value)")
	}
	n2, ok2, err := s.coerceInt(args[3])
	if err != nil {
		return nil, err
	}
	if !ok2 {
		return nil, fmt.Errorf("bad argument #4 to 'upvaluejoin' (number expected, got %s)", s.objTypeName(args[3]))
	}
	if !args[2].IsFunction() {
		return nil, fmt.Errorf("bad argument #3 to 'upvaluejoin' (function expected, got %s)", s.objTypeName(args[2]))
	}
	if !args[2].IsClosure() {
		if n2 < 1 || int(n2) > s.nativeUpvalueCountVal(args[2]) {
			return nil, fmt.Errorf("bad argument #4 to 'upvaluejoin' (invalid upvalue index)")
		}
	} else if n2 < 1 || int(n2) > s.closureUpvalCount(s.asClosure(args[2])) {
		return nil, fmt.Errorf("bad argument #4 to 'upvaluejoin' (invalid upvalue index)")
	}
	if !args[0].IsClosure() {
		return nil, fmt.Errorf("bad argument #1 to 'upvaluejoin' (Lua function expected)")
	}
	if !args[2].IsClosure() {
		return nil, fmt.Errorf("bad argument #3 to 'upvaluejoin' (Lua function expected)")
	}
	c1, c2 := s.asClosure(args[0]), s.asClosure(args[2])
	s.setClosureUpval(c1, int(n1-1), s.closureUpval(c2, int(n2-1)))
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
		mt = s.asTable(args[1])
	}
	s.setMetatableOf(args[0], mt)
	if err := s.ConsumeMemErr(); err != nil {
		return nil, err
	}
	return []Value{args[0]}, nil
}

func nativeDebugGetRegistry(s *State, _ []Value) ([]Value, error) {
	return []Value{TableValue(s.registry())}, nil
}

func nativeDebugGetUserValue(s *State, args []Value) ([]Value, error) {
	n := int64(1)
	if len(args) >= 2 && !args[1].IsNil() {
		var ok, nonInt bool
		var err error
		n, ok, nonInt, err = s.coerceIntKind(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			if nonInt {
				return nil, fmt.Errorf("bad argument #2 to 'getuservalue' (number has no integer representation)")
			}
			return nil, fmt.Errorf("bad argument #2 to 'getuservalue' (number expected, got %s)", s.objTypeName(args[1]))
		}
	}
	if len(args) == 0 || !args[0].IsUserdata() {
		return []Value{NilValue()}, nil
	}
	u := s.asUserdata(args[0])
	if n < 1 || int(n) > s.userdataUValCount(u) {
		return []Value{NilValue()}, nil
	}
	return []Value{s.userdataUValGet(u, int(n)-1), TrueValue()}, nil
}

func nativeDebugSetUserValue(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 || !args[0].IsUserdata() {
		got := "no value"
		if len(args) >= 1 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'setuservalue' (userdata expected, got %s)", got)
	}
	u := s.asUserdata(args[0])
	n := int64(1)
	if len(args) >= 3 && !args[2].IsNil() {
		var err error
		n, _, err = s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
	}
	if n < 1 || int(n) > s.userdataUValCount(u) {
		return []Value{NilValue()}, nil
	}
	s.SetUserdataUValue(u, int(n)-1, args[1])
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
	if len(rest) < 2 || (!rest[1].IsString() && !rest[1].IsNumber()) {
		got := "no value"
		if len(rest) >= 2 {
			got = s.objTypeName(rest[1])
		}
		return nil, fmt.Errorf("bad argument #2 to 'sethook' (string expected, got %s)", got)
	}
	maskStr := s.FormatValue(rest[1])
	if !rest[0].IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'sethook' (function expected, got %s)", s.objTypeName(rest[0]))
	}
	var mask uint8
	cancelFn := s.nativeCancelFunc()
	const allMask = HookMaskCall | HookMaskRet | HookMaskLine
	for i := range len(maskStr) {
		if cancelFn != nil && i > 0 && (i&4095) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		switch maskStr[i] {
		case 'c':
			mask |= HookMaskCall
		case 'r':
			mask |= HookMaskRet
		case 'l':
			mask |= HookMaskLine
		}
		if mask == allMask {
			break
		}
	}
	count := 0
	if len(rest) >= 3 && !rest[2].IsNil() {
		c, ok, err := s.coerceInt(rest[2])
		if err != nil {
			return nil, err
		}
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
	if th == s.cur() && s.topFrameIsNative(th, s.builtinDebugSetHook) {
		idx--
	}
	if idx >= 0 {
		cf := &th.framesSlice(s.arena)[idx]
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
