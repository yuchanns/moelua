package compiler

import (
	"fmt"
	"math"

	"github.com/yuchanns/moelua/bytecode"
)

type blockCnt struct {
	previous   *blockCnt
	firstlabel int
	firstgoto  int
	nactvar    int16
	upval      bool
	isloop     uint8 // 1 = loop, 2 = loop with pending breaks
	insidetbc  bool
}

type funcState struct {
	f            *bytecode.Proto
	prev         *funcState
	ls           *lexState
	bl           *blockCnt
	pc           int
	lasttarget   int
	previousline int
	firstlocal   int
	firstlabel   int
	ndebugvars   int16
	nactvar      int16
	freereg      uint8
	iwthabs      int
	needclose    bool

	// Per-Proto constant deduplication cache (mirrors lcode.c kcache).
	kNil     int
	kFalse   int
	kTrue    int
	kZeroFlt int
	kInt     map[int64]int
	kFlt     map[uint64]int
	kStr     map[string]int
}

func (ls *lexState) enterLevel() {
	ls.nCcalls++
	if ls.nCcalls >= ls.maxRecursion {
		ls.lexError("C stack overflow", 0)
	}
}

func (ls *lexState) leaveLevel() {
	ls.nCcalls--
}

func (ls *lexState) errorExpected(tok int) {
	ls.syntaxError(fmt.Sprintf("%s expected", ls.token2Str(tok)))
}

func (fs *funcState) errorLimit(limit int, what string) {
	line := fs.f.LineDefined
	where := "main function"
	if line != 0 {
		where = fmt.Sprintf("function at line %d", line)
	}
	fs.ls.syntaxError(fmt.Sprintf("too many %s (limit is %d) in %s", what, limit, where))
}

func (fs *funcState) checkLimit(v, l int, what string) {
	if v > l {
		fs.errorLimit(l, what)
	}
}

func (ls *lexState) testNext(c int) bool {
	if ls.t.tok == c {
		ls.next()
		return true
	}
	return false
}

func (ls *lexState) check(c int) {
	if ls.t.tok != c {
		ls.errorExpected(c)
	}
}

func (ls *lexState) checkNext(c int) {
	ls.check(c)
	ls.next()
}

func (ls *lexState) checkCondition(c bool, msg string) {
	if !c {
		ls.syntaxError(msg)
	}
}

func (ls *lexState) checkMatch(what, who, where int) {
	if !ls.testNext(what) {
		if where == ls.linenumber {
			ls.errorExpected(what)
		} else {
			ls.syntaxError(fmt.Sprintf("%s expected (to close %s at line %d)",
				ls.token2Str(what), ls.token2Str(who), where))
		}
	}
}

func (ls *lexState) strCheckName() string {
	ls.check(tkName)
	s := ls.t.s
	ls.next()
	return s
}

func (ls *lexState) codeName(e *expDesc) {
	codeString(e, ls.strCheckName())
}

func (fs *funcState) registerLocalVar(varname string) int16 {
	fs.checkLimit(int(fs.ndebugvars)+1, math.MaxInt16, "local variables")
	fs.f.LocVars = append(fs.f.LocVars, bytecode.LocVar{
		Name:    varname,
		StartPC: fs.pc,
	})
	idx := fs.ndebugvars
	fs.ndebugvars++
	return idx
}

func (ls *lexState) newVarKind(name string, hasName bool, kind uint8) int {
	fs := ls.fs
	fs.checkLimit(ls.dyd.actvarN+1, math.MaxInt16, "variable declarations")
	vd := varDesc{
		kind:    kind,
		name:    name,
		hasName: hasName,
	}
	if ls.dyd.actvarN < len(ls.dyd.actvar) {
		ls.dyd.actvar[ls.dyd.actvarN] = vd
	} else {
		ls.dyd.actvar = append(ls.dyd.actvar, vd)
	}
	ls.dyd.actvarN++
	return ls.dyd.actvarN - 1 - fs.firstlocal
}

func (ls *lexState) newLocalVar(name string) int {
	return ls.newVarKind(name, true, vdkReg)
}

func (fs *funcState) getLocalVarDesc(vidx int) *varDesc {
	return &fs.ls.dyd.actvar[fs.firstlocal+vidx]
}

func (fs *funcState) regLevel(nvar int) uint8 {
	for nvar > 0 {
		nvar--
		vd := fs.getLocalVarDesc(nvar)
		if vd.inReg() {
			return vd.ridx + 1
		}
	}
	return 0
}

func (fs *funcState) nVarStack() uint8 {
	return fs.regLevel(int(fs.nactvar))
}

func (fs *funcState) localDebugInfo(vidx int) *bytecode.LocVar {
	vd := fs.getLocalVarDesc(vidx)
	if !vd.inReg() {
		return nil
	}
	return &fs.f.LocVars[vd.pidx]
}

func (fs *funcState) initVar(e *expDesc, vidx int) {
	e.f = noJump
	e.t = noJump
	e.k = vLocal
	e.varInfo.vidx = int16(vidx)
	e.varInfo.ridx = fs.getLocalVarDesc(vidx).ridx
}

func (ls *lexState) checkReadOnly(e *expDesc) {
	fs := ls.fs
	varname := ""
	hasErr := false
	switch e.k {
	case vConst:
		varname = ls.dyd.actvar[e.info].name
		hasErr = true
	case vLocal, vVargVar:
		vd := fs.getLocalVarDesc(int(e.varInfo.vidx))
		if vd.kind != vdkReg {
			varname = vd.name
			hasErr = true
		}
	case vUpval:
		up := &fs.f.Upvalues[e.info]
		if up.Kind != vdkReg {
			varname = up.Name
			hasErr = true
		}
	case vVargInd:
		fs.needVaTab()
		e.k = vIndexed
		fallthrough
	case vIndexUp, vIndexStr, vIndexed:
		if e.ind.ro != 0 {
			varname = fs.f.Consts[e.ind.keystr].Str
			hasErr = true
		}
	default:
		// vIndexI cannot be read-only
		return
	}
	if hasErr {
		ls.semError("attempt to assign to const variable '%s'", varname)
	}
}

func (ls *lexState) adjustLocalVars(nvars int) {
	fs := ls.fs
	reglvl := int(fs.nVarStack())
	for range nvars {
		vidx := int(fs.nactvar)
		fs.nactvar++
		vd := fs.getLocalVarDesc(vidx)
		vd.ridx = uint8(reglvl)
		reglvl++
		vd.pidx = fs.registerLocalVar(vd.name)
		fs.checkLimit(reglvl, maxVars, "local variables")
	}
}

func (fs *funcState) removeVars(tolevel int) {
	fs.ls.dyd.actvarN -= int(fs.nactvar) - tolevel
	for int(fs.nactvar) > tolevel {
		fs.nactvar--
		if lv := fs.localDebugInfo(int(fs.nactvar)); lv != nil {
			lv.EndPC = fs.pc
		}
	}
}

func (fs *funcState) searchUpvalue(name string) int {
	for i := range fs.f.Upvalues {
		if fs.f.Upvalues[i].Name == name {
			return i
		}
	}
	return -1
}

func (fs *funcState) allocUpvalue() *bytecode.UpvalDesc {
	fs.checkLimit(len(fs.f.Upvalues)+1, maxUpval, "upvalues")
	fs.f.Upvalues = append(fs.f.Upvalues, bytecode.UpvalDesc{})
	return &fs.f.Upvalues[len(fs.f.Upvalues)-1]
}

func (fs *funcState) newUpvalue(name string, v *expDesc) int {
	up := fs.allocUpvalue()
	prev := fs.prev
	if v.k == vLocal {
		up.InStack = 1
		up.Idx = v.varInfo.ridx
		up.Kind = prev.getLocalVarDesc(int(v.varInfo.vidx)).kind
	} else {
		up.InStack = 0
		up.Idx = uint8(v.info)
		up.Kind = prev.f.Upvalues[v.info].Kind
	}
	up.Name = name
	return len(fs.f.Upvalues) - 1
}

func (fs *funcState) searchVar(n string, v *expDesc) int {
	for i := int(fs.nactvar) - 1; i >= 0; i-- {
		vd := fs.getLocalVarDesc(i)
		if vd.isGlobal() {
			if !vd.hasName { // collective global *
				if v.info < 0 {
					v.info = fs.firstlocal + i
				}
			} else {
				if vd.name == n {
					initExp(v, vGlobal, fs.firstlocal+i)
					return int(vGlobal)
				} else if v.info == -1 {
					v.info = -2
				}
			}
		} else if vd.hasName && vd.name == n {
			if vd.kind == rdkCTC {
				initExp(v, vConst, fs.firstlocal+i)
			} else {
				fs.initVar(v, i)
				if vd.kind == rdkVaVar {
					v.k = vVargVar
				}
			}
			return int(v.k)
		}
	}
	return -1
}

func (fs *funcState) markUpval(level int) {
	bl := fs.bl
	for int(bl.nactvar) > level {
		bl = bl.previous
	}
	bl.upval = true
	fs.needclose = true
}

func (fs *funcState) markToBeClosed() {
	bl := fs.bl
	bl.upval = true
	bl.insidetbc = true
	fs.needclose = true
}

func singleVarAux(fs *funcState, n string, v *expDesc, base bool) {
	if fs.searchVar(n, v) >= 0 {
		if !base {
			if v.k == vVargVar {
				fs.vapar2Local(v)
			}
			if v.k == vLocal {
				fs.markUpval(int(v.varInfo.vidx))
			}
		}
		return
	}
	idx := fs.searchUpvalue(n)
	if idx < 0 {
		if fs.prev != nil {
			singleVarAux(fs.prev, n, v, false)
		}
		if v.k == vLocal || v.k == vUpval {
			idx = fs.newUpvalue(n, v)
		} else {
			return // global or compile-time constant
		}
	}
	initExp(v, vUpval, idx)
}

func (ls *lexState) buildGlobal(varname string, v *expDesc) {
	fs := ls.fs
	var key expDesc
	initExp(v, vGlobal, -1)
	singleVarAux(fs, luaEnv, v, true)
	if v.k == vGlobal {
		ls.semError("%s is global when accessing variable '%s'", luaEnv, varname)
	}
	fs.exp2AnyRegUp(v)
	codeString(&key, varname)
	fs.indexed(v, &key)
}

func (ls *lexState) buildVar(varname string, v *expDesc) {
	initExp(v, vGlobal, -1)
	singleVarAux(ls.fs, varname, v, true)
	if v.k == vGlobal {
		info := v.info
		if info == -2 {
			ls.semError("variable '%s' not declared", varname)
		}
		ls.buildGlobal(varname, v)
		if info != -1 && ls.dyd.actvar[info].kind == gdkConst {
			v.ind.ro = 1
		}
	}
}

func (ls *lexState) singleVar(v *expDesc) {
	ls.buildVar(ls.strCheckName(), v)
}

func (ls *lexState) adjustAssign(nvars, nexps int, e *expDesc) {
	fs := ls.fs
	needed := nvars - nexps
	fs.checkStack(needed)
	if e.k.hasMultRet() {
		extra := needed + 1
		if extra < 0 {
			extra = 0
		}
		fs.setReturns(e, extra)
	} else {
		if e.k != vVoid {
			fs.exp2NextReg(e)
		}
		if needed > 0 {
			fs.codeNil(int(fs.freereg), needed)
		}
	}
	if needed > 0 {
		fs.reserveRegs(needed)
	} else {
		fs.freereg = uint8(int(fs.freereg) + needed)
	}
}

func (ls *lexState) jumpScopeError(gt *labelDesc) {
	vd := ls.fs.getLocalVarDesc(int(gt.nactvar))
	varname := "*"
	if vd.hasName {
		varname = vd.name
	}
	ls.semError("<goto %s> at line %d jumps into the scope of '%s'", gt.name, gt.line, varname)
}

func (ls *lexState) closeGoto(g int, label *labelDesc, bup bool) {
	fs := ls.fs
	gt := ls.dyd.gt[g]
	if gt.nactvar < label.nactvar {
		ls.jumpScopeError(&gt)
	}
	if gt.close || (label.nactvar < gt.nactvar && bup) {
		stklevel := fs.regLevel(int(label.nactvar))
		fs.f.Code[gt.pc+1] = fs.f.Code[gt.pc]
		fs.f.Code[gt.pc] = createABCk(bytecode.OP_CLOSE, int(stklevel), 0, 0, 0)
		gt.pc++
	}
	fs.patchList(gt.pc, label.pc)
	copy(ls.dyd.gt[g:], ls.dyd.gt[g+1:])
	ls.dyd.gt = ls.dyd.gt[:len(ls.dyd.gt)-1]
}

func (ls *lexState) findLabel(name string, ilb int) *labelDesc {
	for ; ilb < len(ls.dyd.label); ilb++ {
		if ls.dyd.label[ilb].name == name {
			return &ls.dyd.label[ilb]
		}
	}
	return nil
}

func (ls *lexState) newLabelEntry(list *[]labelDesc, name string, line, pc int) int {
	n := len(*list)
	ls.fs.checkLimit(n+1, math.MaxInt16, "labels/gotos")
	*list = append(*list, labelDesc{
		name:    name,
		line:    line,
		nactvar: ls.fs.nactvar,
		close:   false,
		pc:      pc,
	})
	return n
}

func (ls *lexState) newGotoEntry(name string, line int) int {
	fs := ls.fs
	pc := fs.jump()
	fs.codeABC(bytecode.OP_CLOSE, 0, 1, 0) // dead placeholder
	return ls.newLabelEntry(&ls.dyd.gt, name, line, pc)
}

func (ls *lexState) createLabel(name string, line int, last bool) {
	fs := ls.fs
	l := ls.newLabelEntry(&ls.dyd.label, name, line, fs.getLabel())
	if last {
		ls.dyd.label[l].nactvar = fs.bl.nactvar
	}
}

func (fs *funcState) solveGotos(bl *blockCnt) {
	ls := fs.ls
	outlevel := fs.regLevel(int(bl.nactvar))
	igt := bl.firstgoto
	for igt < len(ls.dyd.gt) {
		gt := &ls.dyd.gt[igt]
		lb := ls.findLabel(gt.name, bl.firstlabel)
		if lb != nil {
			ls.closeGoto(igt, lb, bl.upval)
		} else {
			if bl.upval && fs.regLevel(int(gt.nactvar)) > outlevel {
				gt.close = true
			}
			gt.nactvar = bl.nactvar
			igt++
		}
	}
	ls.dyd.label = ls.dyd.label[:bl.firstlabel]
}

func (fs *funcState) enterBlock(bl *blockCnt, isloop uint8) {
	bl.isloop = isloop
	bl.nactvar = fs.nactvar
	bl.firstlabel = len(fs.ls.dyd.label)
	bl.firstgoto = len(fs.ls.dyd.gt)
	bl.upval = false
	bl.insidetbc = fs.bl != nil && fs.bl.insidetbc
	bl.previous = fs.bl
	fs.bl = bl
}

func (ls *lexState) undefGoto(gt *labelDesc) {
	ls.semError("no visible label '%s' for <goto> at line %d", gt.name, gt.line)
}

func (fs *funcState) leaveBlock() {
	bl := fs.bl
	ls := fs.ls
	stklevel := fs.regLevel(int(bl.nactvar))
	if bl.previous != nil && bl.upval {
		fs.codeABC(bytecode.OP_CLOSE, int(stklevel), 0, 0)
	}
	fs.freereg = stklevel
	fs.removeVars(int(bl.nactvar))
	if bl.isloop == 2 {
		ls.createLabel("break", 0, false)
	}
	fs.solveGotos(bl)
	if bl.previous == nil {
		if bl.firstgoto < len(ls.dyd.gt) {
			ls.undefGoto(&ls.dyd.gt[bl.firstgoto])
		}
	}
	fs.bl = bl.previous
}

func (ls *lexState) addPrototype() *bytecode.Proto {
	fs := ls.fs
	fs.checkLimit(len(fs.f.Protos)+1, bytecode.MaxArgBx+1, "functions")
	clp := &bytecode.Proto{}
	fs.f.Protos = append(fs.f.Protos, clp)
	return clp
}

func (ls *lexState) codeClosure(v *expDesc) {
	fs := ls.fs.prev
	initExp(v, vReloc, fs.codeABx(bytecode.OP_CLOSURE, 0, len(fs.f.Protos)-1))
	fs.exp2NextReg(v)
}

func (ls *lexState) openFunc(fs *funcState, bl *blockCnt) {
	f := fs.f
	fs.prev = ls.fs
	fs.ls = ls
	ls.fs = fs
	fs.pc = 0
	fs.previousline = f.LineDefined
	fs.iwthabs = 0
	fs.lasttarget = 0
	fs.freereg = 0
	fs.ndebugvars = 0
	fs.nactvar = 0
	fs.needclose = false
	fs.firstlocal = ls.dyd.actvarN
	fs.firstlabel = len(ls.dyd.label)
	fs.bl = nil
	fs.kNil = -1
	fs.kFalse = -1
	fs.kTrue = -1
	fs.kZeroFlt = -1
	fs.kInt = make(map[int64]int)
	fs.kFlt = make(map[uint64]int)
	fs.kStr = make(map[string]int)
	f.Source = ls.source
	f.MaxStackSize = 2
	fs.enterBlock(bl, 0)
}

func (ls *lexState) closeFunc() {
	fs := ls.fs
	fs.ret(int(fs.nVarStack()), 0)
	fs.leaveBlock()
	fs.finish()
	ls.fs = fs.prev
}

func (ls *lexState) blockFollow(withUntil bool) bool {
	switch ls.t.tok {
	case tkElse, tkElseif, tkEnd, tkEOS:
		return true
	case tkUntil:
		return withUntil
	default:
		return false
	}
}

func (ls *lexState) statlist() {
	for !ls.blockFollow(true) {
		if ls.t.tok == tkReturn {
			ls.statement()
			return
		}
		ls.statement()
	}
}

func (ls *lexState) fieldSel(v *expDesc) {
	fs := ls.fs
	var key expDesc
	fs.exp2AnyRegUp(v)
	ls.next() // skip '.' or ':'
	ls.codeName(&key)
	fs.indexed(v, &key)
}

func (ls *lexState) yIndex(v *expDesc) {
	ls.next() // skip '['
	ls.expr(v)
	ls.fs.exp2Val(v)
	ls.checkNext(']')
}

// Table constructors.
type consControl struct {
	v          expDesc
	t          *expDesc
	nh         int
	na         int
	tostore    int
	maxtostore int
}

func (ls *lexState) recField(cc *consControl) {
	fs := ls.fs
	reg := fs.freereg
	var key, val expDesc
	if ls.t.tok == tkName {
		ls.codeName(&key)
	} else {
		ls.yIndex(&key)
	}
	cc.nh++
	ls.checkNext('=')
	tab := *cc.t
	fs.indexed(&tab, &key)
	ls.expr(&val)
	fs.storeVar(&tab, &val)
	fs.freereg = reg
}

func closeListField(fs *funcState, cc *consControl) {
	fs.exp2NextReg(&cc.v)
	cc.v.k = vVoid
	if cc.tostore >= cc.maxtostore {
		fs.setList(cc.t.info, cc.na, cc.tostore)
		cc.na += cc.tostore
		cc.tostore = 0
	}
}

func lastListField(fs *funcState, cc *consControl) {
	if cc.tostore == 0 {
		return
	}
	if cc.v.k.hasMultRet() {
		fs.setMultRet(&cc.v)
		fs.setList(cc.t.info, cc.na, luaMultRet)
		cc.na--
	} else {
		if cc.v.k != vVoid {
			fs.exp2NextReg(&cc.v)
		}
		fs.setList(cc.t.info, cc.na, cc.tostore)
	}
	cc.na += cc.tostore
}

func (ls *lexState) listField(cc *consControl) {
	ls.expr(&cc.v)
	cc.tostore++
}

func (ls *lexState) field(cc *consControl) {
	switch ls.t.tok {
	case tkName:
		if ls.lookAhead() != '=' {
			ls.listField(cc)
		} else {
			ls.recField(cc)
		}
	case '[':
		ls.recField(cc)
	default:
		ls.listField(cc)
	}
}

func maxToStore(fs *funcState) int {
	numFreeRegs := maxFStack - int(fs.freereg)
	if numFreeRegs >= 160 {
		return numFreeRegs / 5
	}
	if numFreeRegs >= 80 {
		return 10
	}
	return 1
}

func (ls *lexState) constructor(t *expDesc) {
	fs := ls.fs
	line := ls.linenumber
	pc := fs.codevABCk(bytecode.OP_NEWTABLE, 0, 0, 0, 0)
	fs.code(0) // space for EXTRAARG
	var cc consControl
	cc.t = t
	initExp(t, vNonReloc, int(fs.freereg))
	fs.reserveRegs(1)
	initExp(&cc.v, vVoid, 0)
	ls.checkNext('{')
	cc.maxtostore = maxToStore(fs)
	for {
		if ls.t.tok == '}' {
			break
		}
		if cc.v.k != vVoid {
			closeListField(fs, &cc)
		}
		ls.field(&cc)
		fs.checkLimit(cc.tostore+cc.na+cc.nh, maxCnst, "items in a constructor")
		if !ls.testNext(',') && !ls.testNext(';') {
			break
		}
	}
	ls.checkMatch('}', '{', line)
	lastListField(fs, &cc)
	fs.setTableSize(pc, t.info, cc.na, cc.nh)
}

func (fs *funcState) setVararg() {
	fs.f.Flag |= bytecode.PFVarargHidden
	fs.codeABC(bytecode.OP_VARARGPREP, 0, 0, 0)
}

func (ls *lexState) parList() {
	fs := ls.fs
	f := fs.f
	nparams := 0
	varargk := false
	if ls.t.tok != ')' {
		for {
			switch ls.t.tok {
			case tkName:
				ls.newLocalVar(ls.strCheckName())
				nparams++
			case tkDots:
				varargk = true
				ls.next()
				if ls.t.tok == tkName {
					ls.newVarKind(ls.strCheckName(), true, rdkVaVar)
				} else {
					ls.newLocalVar("(vararg table)")
				}
			default:
				ls.syntaxError("<name> or '...' expected")
			}
			if varargk || !ls.testNext(',') {
				break
			}
		}
	}
	ls.adjustLocalVars(nparams)
	f.NumParams = uint8(fs.nactvar)
	if varargk {
		fs.setVararg()
		ls.adjustLocalVars(1)
	}
	fs.reserveRegs(int(fs.nactvar))
}

func (ls *lexState) body(e *expDesc, isMethod bool, line int) {
	var newFS funcState
	var bl blockCnt
	newFS.f = ls.addPrototype()
	newFS.f.LineDefined = line
	ls.openFunc(&newFS, &bl)
	ls.checkNext('(')
	if isMethod {
		ls.newLocalVar("self")
		ls.adjustLocalVars(1)
	}
	ls.parList()
	ls.checkNext(')')
	ls.statlist()
	newFS.f.LastLineDefined = ls.linenumber
	ls.checkMatch(tkEnd, tkFunction, line)
	ls.codeClosure(e)
	ls.closeFunc()
}

func (ls *lexState) expList(v *expDesc) int {
	n := 1
	ls.expr(v)
	for ls.testNext(',') {
		ls.fs.exp2NextReg(v)
		ls.expr(v)
		n++
	}
	return n
}

func (ls *lexState) funcArgs(f *expDesc) {
	fs := ls.fs
	var args expDesc
	line := ls.linenumber
	switch ls.t.tok {
	case '(':
		ls.next()
		if ls.t.tok == ')' {
			args.k = vVoid
		} else {
			ls.expList(&args)
			if args.k.hasMultRet() {
				fs.setMultRet(&args)
			}
		}
		ls.checkMatch(')', '(', line)
	case '{':
		ls.constructor(&args)
	case tkString:
		codeString(&args, ls.t.s)
		ls.next()
	default:
		ls.syntaxError("function arguments expected")
	}
	base := f.info
	var nparams int
	if args.k.hasMultRet() {
		nparams = luaMultRet
	} else {
		if args.k != vVoid {
			fs.exp2NextReg(&args)
		}
		nparams = int(fs.freereg) - (base + 1)
	}
	initExp(f, vCall, fs.codeABC(bytecode.OP_CALL, base, nparams+1, 2))
	fs.fixLine(line)
	fs.freereg = uint8(base + 1)
}

func (ls *lexState) primaryExp(v *expDesc) {
	switch ls.t.tok {
	case '(':
		line := ls.linenumber
		ls.next()
		ls.expr(v)
		ls.checkMatch(')', '(', line)
		ls.fs.dischargeVars(v)
	case tkName:
		ls.singleVar(v)
	default:
		ls.syntaxError("unexpected symbol")
	}
}

func (ls *lexState) suffixedExp(v *expDesc) {
	fs := ls.fs
	ls.primaryExp(v)
	for {
		switch ls.t.tok {
		case '.':
			ls.fieldSel(v)
		case '[':
			var key expDesc
			fs.exp2AnyRegUp(v)
			ls.yIndex(&key)
			fs.indexed(v, &key)
		case ':':
			var key expDesc
			ls.next()
			ls.codeName(&key)
			fs.codeSelf(v, &key)
			ls.funcArgs(v)
		case '(', tkString, '{':
			fs.exp2NextReg(v)
			ls.funcArgs(v)
		default:
			return
		}
	}
}

func (ls *lexState) simpleExp(v *expDesc) {
	switch ls.t.tok {
	case tkFlt:
		initExp(v, vKFlt, 0)
		v.nval = ls.t.r
	case tkInt:
		initExp(v, vKInt, 0)
		v.ival = ls.t.i
	case tkString:
		codeString(v, ls.t.s)
	case tkNil:
		initExp(v, vNil, 0)
	case tkTrue:
		initExp(v, vTrue, 0)
	case tkFalse:
		initExp(v, vFalse, 0)
	case tkDots:
		fs := ls.fs
		ls.checkCondition(fs.f.IsVararg(), "cannot use '...' outside a vararg function")
		initExp(v, vVararg, fs.codeABC(bytecode.OP_VARARG, 0, int(fs.f.NumParams), 1))
	case '{':
		ls.constructor(v)
		return
	case tkFunction:
		ls.next()
		ls.body(v, false, ls.linenumber)
		return
	default:
		ls.suffixedExp(v)
		return
	}
	ls.next()
}

func getUnOpr(tok int) unOpr {
	switch tok {
	case tkNot:
		return oprNot
	case '-':
		return oprMinus
	case '~':
		return oprBNot
	case '#':
		return oprLen
	default:
		return oprNoUnOpr
	}
}

func getBinOpr(tok int) binOpr {
	switch tok {
	case '+':
		return oprAdd
	case '-':
		return oprSub
	case '*':
		return oprMul
	case '%':
		return oprMod
	case '^':
		return oprPow
	case '/':
		return oprDiv
	case tkIdiv:
		return oprIDiv
	case '&':
		return oprBAnd
	case '|':
		return oprBOr
	case '~':
		return oprBXor
	case tkShl:
		return oprShl
	case tkShr:
		return oprShr
	case tkConcat:
		return oprConcat
	case tkNe:
		return oprNE
	case tkEq:
		return oprEQ
	case '<':
		return oprLT
	case tkLe:
		return oprLE
	case '>':
		return oprGT
	case tkGe:
		return oprGE
	case tkAnd:
		return oprAnd
	case tkOr:
		return oprOr
	default:
		return oprNoBinOpr
	}
}

var priority = [...]struct {
	left  uint8
	right uint8
}{
	oprAdd:    {10, 10},
	oprSub:    {10, 10},
	oprMul:    {11, 11},
	oprMod:    {11, 11},
	oprPow:    {14, 13},
	oprDiv:    {11, 11},
	oprIDiv:   {11, 11},
	oprBAnd:   {6, 6},
	oprBOr:    {4, 4},
	oprBXor:   {5, 5},
	oprShl:    {7, 7},
	oprShr:    {7, 7},
	oprConcat: {9, 8},
	oprEQ:     {3, 3},
	oprLT:     {3, 3},
	oprLE:     {3, 3},
	oprNE:     {3, 3},
	oprGT:     {3, 3},
	oprGE:     {3, 3},
	oprAnd:    {2, 2},
	oprOr:     {1, 1},
}

const unaryPriority = 12

func (ls *lexState) subExpr(v *expDesc, limit uint8) binOpr {
	ls.enterLevel()
	uop := getUnOpr(ls.t.tok)
	if uop != oprNoUnOpr {
		line := ls.linenumber
		ls.next()
		ls.subExpr(v, unaryPriority)
		ls.fs.prefix(uop, v, line)
	} else {
		ls.simpleExp(v)
	}
	op := getBinOpr(ls.t.tok)
	for op != oprNoBinOpr && priority[op].left > limit {
		var v2 expDesc
		line := ls.linenumber
		ls.next()
		ls.fs.infix(op, v)
		nextOp := ls.subExpr(&v2, priority[op].right)
		ls.fs.posfix(op, v, &v2, line)
		op = nextOp
	}
	ls.leaveLevel()
	return op
}

func (ls *lexState) expr(v *expDesc) {
	ls.subExpr(v, 0)
}

func (ls *lexState) block() {
	fs := ls.fs
	var bl blockCnt
	fs.enterBlock(&bl, 0)
	ls.statlist()
	fs.leaveBlock()
}

type lhsAssign struct {
	prev *lhsAssign
	v    expDesc
}

func (ls *lexState) checkConflict(lh *lhsAssign, v *expDesc) {
	fs := ls.fs
	extra := fs.freereg
	conflict := false
	for ; lh != nil; lh = lh.prev {
		if lh.v.k.isIndexed() {
			if lh.v.k == vIndexUp {
				if v.k == vUpval && int(lh.v.ind.t) == v.info {
					conflict = true
					lh.v.k = vIndexStr
					lh.v.ind.t = extra
				}
			} else {
				if v.k == vLocal && lh.v.ind.t == v.varInfo.ridx {
					conflict = true
					lh.v.ind.t = extra
				}
				if lh.v.k == vIndexed && v.k == vLocal && int(lh.v.ind.idx) == int(v.varInfo.ridx) {
					conflict = true
					lh.v.ind.idx = int16(extra)
				}
			}
		}
	}
	if conflict {
		if v.k == vLocal {
			fs.codeABC(bytecode.OP_MOVE, int(extra), int(v.varInfo.ridx), 0)
		} else {
			fs.codeABC(bytecode.OP_GETUPVAL, int(extra), v.info, 0)
		}
		fs.reserveRegs(1)
	}
}

func storeVarTop(fs *funcState, v *expDesc) {
	var e expDesc
	initExp(&e, vNonReloc, int(fs.freereg)-1)
	fs.storeVar(v, &e)
}

func (ls *lexState) restAssign(lh *lhsAssign, nvars int) {
	var e expDesc
	ls.checkCondition(lh.v.k.isVar(), "syntax error")
	ls.checkReadOnly(&lh.v)
	if ls.testNext(',') {
		var nv lhsAssign
		nv.prev = lh
		ls.suffixedExp(&nv.v)
		if !nv.v.k.isIndexed() {
			ls.checkConflict(lh, &nv.v)
		}
		ls.enterLevel()
		ls.restAssign(&nv, nvars+1)
		ls.leaveLevel()
	} else {
		ls.checkNext('=')
		nexps := ls.expList(&e)
		if nexps != nvars {
			ls.adjustAssign(nvars, nexps, &e)
		} else {
			ls.fs.setOneRet(&e)
			ls.fs.storeVar(&lh.v, &e)
			return
		}
	}
	storeVarTop(ls.fs, &lh.v)
}

func (ls *lexState) cond() int {
	var v expDesc
	ls.expr(&v)
	if v.k == vNil {
		v.k = vFalse
	}
	ls.fs.goIfTrue(&v)
	return v.f
}

func (ls *lexState) gotoStat(line int) {
	name := ls.strCheckName()
	ls.newGotoEntry(name, line)
}

func (ls *lexState) breakStat(line int) {
	var bl *blockCnt
	for bl = ls.fs.bl; bl != nil; bl = bl.previous {
		if bl.isloop != 0 {
			break
		}
	}
	if bl == nil {
		ls.syntaxError("break outside loop")
	}
	bl.isloop = 2
	ls.next() // skip break
	ls.newGotoEntry("break", line)
}

func (ls *lexState) checkRepeated(name string) {
	if lb := ls.findLabel(name, ls.fs.firstlabel); lb != nil {
		ls.semError("label '%s' already defined on line %d", name, lb.line)
	}
}

func (ls *lexState) labelStat(name string, line int) {
	ls.checkNext(tkDbColon)
	for ls.t.tok == ';' || ls.t.tok == tkDbColon {
		ls.statement()
	}
	ls.checkRepeated(name)
	ls.createLabel(name, line, ls.blockFollow(false))
}

func (ls *lexState) whileStat(line int) {
	fs := ls.fs
	var bl blockCnt
	ls.next() // skip WHILE
	whileInit := fs.getLabel()
	condExit := ls.cond()
	fs.enterBlock(&bl, 1)
	ls.checkNext(tkDo)
	ls.block()
	fs.jumpTo(whileInit)
	ls.checkMatch(tkEnd, tkWhile, line)
	fs.leaveBlock()
	fs.patchToHere(condExit)
}

func (ls *lexState) repeatStat(line int) {
	fs := ls.fs
	repeatInit := fs.getLabel()
	var bl1, bl2 blockCnt
	fs.enterBlock(&bl1, 1) // loop block
	fs.enterBlock(&bl2, 0) // scope block
	ls.next()              // skip REPEAT
	ls.statlist()
	ls.checkMatch(tkUntil, tkRepeat, line)
	condExit := ls.cond()
	if bl2.upval {
		exit := fs.jump()
		fs.patchToHere(condExit)
		fs.codeABC(bytecode.OP_CLOSE, int(fs.regLevel(int(bl2.nactvar))), 0, 0)
		condExit = fs.jump()
		fs.patchToHere(exit)
	}
	fs.patchList(condExit, repeatInit)
	fs.leaveBlock() // finish scope
	fs.leaveBlock() // finish loop
}

func (ls *lexState) exp1() {
	var e expDesc
	ls.expr(&e)
	ls.fs.exp2NextReg(&e)
}

func (fs *funcState) fixForJump(pc, dest int, back bool) {
	offset := dest - (pc + 1)
	if back {
		offset = -offset
	}
	if offset > bytecode.MaxArgBx {
		fs.ls.syntaxError("control structure too long")
	}
	setArgBx(&fs.f.Code[pc], offset)
}

func (ls *lexState) forBody(base, line, nvars int, isGen bool) {
	fs := ls.fs
	var bl blockCnt
	prepOp := bytecode.OP_FORPREP
	loopOp := bytecode.OP_FORLOOP
	if isGen {
		prepOp = bytecode.OP_TFORPREP
		loopOp = bytecode.OP_TFORLOOP
	}
	ls.checkNext(tkDo)
	prep := fs.codeABx(prepOp, base, 0)
	fs.freereg--
	fs.enterBlock(&bl, 0)
	ls.adjustLocalVars(nvars)
	fs.reserveRegs(nvars)
	ls.block()
	fs.leaveBlock()
	fs.fixForJump(prep, fs.getLabel(), false)
	if isGen {
		fs.codeABC(bytecode.OP_TFORCALL, base, 0, nvars)
		fs.fixLine(line)
	}
	endfor := fs.codeABx(loopOp, base, 0)
	fs.fixForJump(endfor, prep+1, true)
	fs.fixLine(line)
}

func (ls *lexState) forNum(varname string, line int) {
	fs := ls.fs
	base := int(fs.freereg)
	ls.newLocalVar("(for state)")
	ls.newLocalVar("(for state)")
	ls.newVarKind(varname, true, rdkConst)
	ls.checkNext('=')
	ls.exp1()
	ls.checkNext(',')
	ls.exp1()
	if ls.testNext(',') {
		ls.exp1()
	} else {
		fs.codeInt(int(fs.freereg), 1)
		fs.reserveRegs(1)
	}
	ls.adjustLocalVars(2)
	ls.forBody(base, line, 1, false)
}

func (ls *lexState) forList(indexname string) {
	fs := ls.fs
	var e expDesc
	nvars := 4
	base := int(fs.freereg)
	ls.newLocalVar("(for state)")
	ls.newLocalVar("(for state)")
	ls.newLocalVar("(for state)")
	ls.newVarKind(indexname, true, rdkConst)
	for ls.testNext(',') {
		ls.newLocalVar(ls.strCheckName())
		nvars++
	}
	ls.checkNext(tkIn)
	line := ls.linenumber
	ls.adjustAssign(4, ls.expList(&e), &e)
	ls.adjustLocalVars(3)
	fs.markToBeClosed()
	fs.checkStack(2)
	ls.forBody(base, line, nvars-3, true)
}

func (ls *lexState) forStat(line int) {
	fs := ls.fs
	var bl blockCnt
	fs.enterBlock(&bl, 1)
	ls.next() // skip 'for'
	varname := ls.strCheckName()
	switch ls.t.tok {
	case '=':
		ls.forNum(varname, line)
	case ',', tkIn:
		ls.forList(varname)
	default:
		ls.syntaxError("'=' or 'in' expected")
	}
	ls.checkMatch(tkEnd, tkFor, line)
	fs.leaveBlock()
}

func (ls *lexState) testThenBlock(escapelist *int) {
	fs := ls.fs
	ls.next() // skip IF or ELSEIF
	condTrue := ls.cond()
	ls.checkNext(tkThen)
	ls.block()
	if ls.t.tok == tkElse || ls.t.tok == tkElseif {
		fs.concat(escapelist, fs.jump())
	}
	fs.patchToHere(condTrue)
}

func (ls *lexState) ifStat(line int) {
	fs := ls.fs
	escapelist := noJump
	ls.testThenBlock(&escapelist)
	for ls.t.tok == tkElseif {
		ls.testThenBlock(&escapelist)
	}
	if ls.testNext(tkElse) {
		ls.block()
	}
	ls.checkMatch(tkEnd, tkIf, line)
	fs.patchToHere(escapelist)
}

func (ls *lexState) localFunc() {
	var b expDesc
	fs := ls.fs
	fvar := int(fs.nactvar)
	ls.newLocalVar(ls.strCheckName())
	ls.adjustLocalVars(1)
	ls.body(&b, false, ls.linenumber)
	fs.localDebugInfo(fvar).StartPC = fs.pc
}

func (ls *lexState) getVarAttribute(df uint8) uint8 {
	if ls.testNext('<') {
		attr := ls.strCheckName()
		ls.checkNext('>')
		switch attr {
		case "const":
			return rdkConst
		case "close":
			return rdkToClose
		default:
			ls.semError("unknown attribute '%s'", attr)
		}
	}
	return df
}

func (fs *funcState) checkToClose(level int) {
	if level != -1 {
		fs.markToBeClosed()
		fs.codeABC(bytecode.OP_TBC, int(fs.regLevel(level)), 0, 0)
	}
}

func (ls *lexState) localStat() {
	fs := ls.fs
	toclose := -1
	var vidx int
	nvars := 0
	var nexps int
	var e expDesc
	defkind := ls.getVarAttribute(vdkReg)
	for {
		vname := ls.strCheckName()
		kind := ls.getVarAttribute(defkind)
		vidx = ls.newVarKind(vname, true, kind)
		if kind == rdkToClose {
			if toclose != -1 {
				ls.semError("multiple to-be-closed variables in local list")
			}
			toclose = int(fs.nactvar) + nvars
		}
		nvars++
		if !ls.testNext(',') {
			break
		}
	}
	if ls.testNext('=') {
		nexps = ls.expList(&e)
	} else {
		e.k = vVoid
		nexps = 0
	}
	vd := fs.getLocalVarDesc(vidx)
	if nvars == nexps && vd.kind == rdkConst {
		if kval, ok := fs.exp2Const(&e); ok {
			vd.kind = rdkCTC
			vd.kVal = kval
			ls.adjustLocalVars(nvars - 1)
			fs.nactvar++
			fs.checkToClose(toclose)
			return
		}
	}
	ls.adjustAssign(nvars, nexps, &e)
	ls.adjustLocalVars(nvars)
	fs.checkToClose(toclose)
}

func (ls *lexState) getGlobalAttribute(df uint8) uint8 {
	kind := ls.getVarAttribute(df)
	switch kind {
	case rdkToClose:
		ls.semError("global variables cannot be to-be-closed")
	case rdkConst:
		return gdkConst
	}
	return kind
}

func (ls *lexState) checkGlobal(varname string, line int) {
	var v expDesc
	ls.buildGlobal(varname, &v)
	k := v.ind.keystr
	ls.fs.codeCheckGlobal(&v, k, line)
}

func (ls *lexState) initGlobal(nvars, firstidx, n, line int) {
	if n == nvars {
		var e expDesc
		nexps := ls.expList(&e)
		ls.adjustAssign(nvars, nexps, &e)
	} else {
		fs := ls.fs
		var v expDesc
		varname := fs.getLocalVarDesc(firstidx + n).name
		ls.buildGlobal(varname, &v)
		ls.enterLevel()
		ls.initGlobal(nvars, firstidx, n+1, line)
		ls.leaveLevel()
		ls.checkGlobal(varname, line)
		storeVarTop(fs, &v)
	}
}

func (ls *lexState) globalNames(defkind uint8) {
	fs := ls.fs
	nvars := 0
	lastidx := 0
	for {
		vname := ls.strCheckName()
		kind := ls.getGlobalAttribute(defkind)
		lastidx = ls.newVarKind(vname, true, kind)
		nvars++
		if !ls.testNext(',') {
			break
		}
	}
	if ls.testNext('=') {
		ls.initGlobal(nvars, lastidx-nvars+1, 0, ls.linenumber)
	}
	fs.nactvar = int16(int(fs.nactvar) + nvars)
}

func (ls *lexState) globalStat() {
	fs := ls.fs
	defkind := ls.getGlobalAttribute(gdkReg)
	if !ls.testNext('*') {
		ls.globalNames(defkind)
	} else {
		ls.newVarKind("", false, defkind)
		fs.nactvar++
	}
}

func (ls *lexState) globalFunc(line int) {
	var v, b expDesc
	fs := ls.fs
	fname := ls.strCheckName()
	ls.newVarKind(fname, true, gdkReg)
	fs.nactvar++
	ls.buildGlobal(fname, &v)
	ls.body(&b, false, ls.linenumber)
	ls.checkGlobal(fname, line)
	fs.storeVar(&v, &b)
	fs.fixLine(line)
}

func (ls *lexState) globalStatFunc(line int) {
	ls.next() // skip 'global'
	if ls.testNext(tkFunction) {
		ls.globalFunc(line)
	} else {
		ls.globalStat()
	}
}

func (ls *lexState) funcName(v *expDesc) bool {
	isMethod := false
	ls.singleVar(v)
	for ls.t.tok == '.' {
		ls.fieldSel(v)
	}
	if ls.t.tok == ':' {
		isMethod = true
		ls.fieldSel(v)
	}
	return isMethod
}

func (ls *lexState) funcStat(line int) {
	var v, b expDesc
	ls.next() // skip FUNCTION
	isMethod := ls.funcName(&v)
	ls.checkReadOnly(&v)
	ls.body(&b, isMethod, line)
	ls.fs.storeVar(&v, &b)
	ls.fs.fixLine(line)
}

func (ls *lexState) exprStat() {
	fs := ls.fs
	var v lhsAssign
	ls.suffixedExp(&v.v)
	if ls.t.tok == '=' || ls.t.tok == ',' {
		v.prev = nil
		ls.restAssign(&v, 1)
	} else {
		ls.checkCondition(v.v.k == vCall, "syntax error")
		setArgC(&fs.f.Code[v.v.info], 1)
	}
}

func (ls *lexState) retStat() {
	fs := ls.fs
	var e expDesc
	var nret int
	first := int(fs.nVarStack())
	if ls.blockFollow(true) || ls.t.tok == ';' {
		nret = 0
	} else {
		nret = ls.expList(&e)
		if e.k.hasMultRet() {
			fs.setMultRet(&e)
			if e.k == vCall && nret == 1 && !fs.bl.insidetbc {
				setOp(&fs.f.Code[e.info], bytecode.OP_TAILCALL)
			}
			nret = luaMultRet
		} else {
			if nret == 1 {
				first = fs.exp2AnyReg(&e)
			} else {
				fs.exp2NextReg(&e)
			}
		}
	}
	fs.ret(first, nret)
	ls.testNext(';')
}

func (ls *lexState) statement() {
	line := ls.linenumber
	ls.enterLevel()
	switch ls.t.tok {
	case ';':
		ls.next()
	case tkIf:
		ls.ifStat(line)
	case tkWhile:
		ls.whileStat(line)
	case tkDo:
		ls.next()
		ls.block()
		ls.checkMatch(tkEnd, tkDo, line)
	case tkFor:
		ls.forStat(line)
	case tkRepeat:
		ls.repeatStat(line)
	case tkFunction:
		ls.funcStat(line)
	case tkLocal:
		ls.next()
		if ls.testNext(tkFunction) {
			ls.localFunc()
		} else {
			ls.localStat()
		}
	case tkGlobal:
		ls.globalStatFunc(line)
	case tkDbColon:
		ls.next()
		ls.labelStat(ls.strCheckName(), line)
	case tkReturn:
		ls.next()
		ls.retStat()
	case tkBreak:
		ls.breakStat(line)
	case tkGoto:
		ls.next()
		ls.gotoStat(line)
	case tkName:
		if ls.compatGlobal && ls.t.s == "global" {
			lk := ls.lookAhead()
			if lk == '<' || lk == tkName || lk == '*' || lk == tkFunction {
				ls.globalStatFunc(line)
				break
			}
		}
		ls.exprStat()
	default:
		ls.exprStat()
	}
	ls.fs.freereg = ls.fs.nVarStack()
	ls.leaveLevel()
}

func (ls *lexState) parseMain() *bytecode.Proto {
	var fs funcState
	var bl blockCnt
	fs.f = &bytecode.Proto{Source: ls.source}
	ls.openFunc(&fs, &bl)
	fs.setVararg()
	env := fs.allocUpvalue()
	env.InStack = 1
	env.Idx = 0
	env.Kind = vdkReg
	env.Name = luaEnv
	ls.next()
	ls.statlist()
	ls.check(tkEOS)
	ls.closeFunc()
	return fs.f
}
