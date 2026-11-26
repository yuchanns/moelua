package compiler

import (
	"math"

	"github.com/yuchanns/moelua/bytecode"
)

func (fs *funcState) const2Val(e *expDesc) bytecode.Constant {
	return fs.ls.dyd.actvar[e.info].kVal
}

func (fs *funcState) exp2Const(e *expDesc) (bytecode.Constant, bool) {
	if e.hasJumps() {
		return bytecode.Constant{}, false
	}
	switch e.k {
	case vFalse:
		return bytecode.Constant{Kind: bytecode.ConstFalse}, true
	case vTrue:
		return bytecode.Constant{Kind: bytecode.ConstTrue}, true
	case vNil:
		return bytecode.Constant{Kind: bytecode.ConstNil}, true
	case vKStr:
		return bytecode.Constant{Kind: bytecode.ConstString, Str: e.strval, IsShort: len(e.strval) <= maxShortStrLen}, true
	case vConst:
		return fs.const2Val(e), true
	default:
		return toNumeral(e)
	}
}

func (fs *funcState) hasPreviousInst() bool {
	return fs.pc > fs.lasttarget
}

func (fs *funcState) codeNil(from, n int) {
	l := from + n - 1
	if fs.hasPreviousInst() {
		prev := &fs.f.Code[fs.pc-1]
		if prev.Op() == bytecode.OP_LOADNIL {
			pfrom := prev.A()
			pl := pfrom + prev.B()
			if (pfrom <= from && from <= pl+1) || (from <= pfrom && pfrom <= l+1) {
				if pfrom < from {
					from = pfrom
				}
				if pl > l {
					l = pl
				}
				setArgA(prev, from)
				setArgB(prev, l-from)
				return
			}
		}
	}
	fs.codeABC(bytecode.OP_LOADNIL, from, n-1, 0)
}

func (fs *funcState) getJump(pc int) int {
	offset := fs.f.Code[pc].SJ()
	if offset == noJump {
		return noJump
	}
	return (pc + 1) + offset
}

func (fs *funcState) fixJump(pc, dest int) {
	offset := dest - (pc + 1)
	if !(-bytecode.OffsetSJ <= offset && offset <= bytecode.MaxArgsJ-bytecode.OffsetSJ) {
		fs.ls.syntaxError("control structure too long")
	}
	setArgSJ(&fs.f.Code[pc], offset)
}

func (fs *funcState) concat(l1 *int, l2 int) {
	if l2 == noJump {
		return
	}
	if *l1 == noJump {
		*l1 = l2
		return
	}
	list := *l1
	for {
		next := fs.getJump(list)
		if next == noJump {
			break
		}
		list = next
	}
	fs.fixJump(list, l2)
}

func (fs *funcState) jump() int {
	return fs.codeSJ(bytecode.OP_JMP, noJump)
}

func (fs *funcState) jumpTo(target int) {
	fs.patchList(fs.jump(), target)
}

func (fs *funcState) ret(first, nret int) {
	var op bytecode.OpCode
	switch nret {
	case 0:
		op = bytecode.OP_RETURN0
	case 1:
		op = bytecode.OP_RETURN1
	default:
		op = bytecode.OP_RETURN
	}
	fs.checkLimit(nret+1, bytecode.MaxArgB, "returns")
	fs.codeABC(op, first, nret+1, 0)
}

func (fs *funcState) condJump(op bytecode.OpCode, a, b, c, k int) int {
	fs.codeABCk(op, a, b, c, k)
	return fs.jump()
}

func (fs *funcState) getLabel() int {
	fs.lasttarget = fs.pc
	return fs.pc
}

func (fs *funcState) getJumpControlPC(pc int) int {
	if pc >= 1 && testTMode(fs.f.Code[pc-1].Op()) {
		return pc - 1
	}
	return pc
}

func (fs *funcState) patchTestReg(node, reg int) bool {
	idx := fs.getJumpControlPC(node)
	inst := &fs.f.Code[idx]
	if inst.Op() != bytecode.OP_TESTSET {
		return false
	}
	if reg != noReg && reg != inst.B() {
		setArgA(inst, reg)
	} else {
		*inst = createABCk(bytecode.OP_TEST, inst.B(), 0, 0, inst.KBit())
	}
	return true
}

func (fs *funcState) removeValues(list int) {
	for list != noJump {
		fs.patchTestReg(list, noReg)
		list = fs.getJump(list)
	}
}

func (fs *funcState) patchListAux(list, vtarget, reg, dtarget int) {
	for list != noJump {
		next := fs.getJump(list)
		if fs.patchTestReg(list, reg) {
			fs.fixJump(list, vtarget)
		} else {
			fs.fixJump(list, dtarget)
		}
		list = next
	}
}

func (fs *funcState) patchList(list, target int) {
	fs.patchListAux(list, target, noReg, target)
}

func (fs *funcState) patchToHere(list int) {
	hr := fs.getLabel()
	fs.patchList(list, hr)
}

func (fs *funcState) saveLineInfo(line int) {
	f := fs.f
	linedif := line - fs.previousline
	pc := fs.pc - 1
	needAbs := linedif <= -limLineDiff || linedif >= limLineDiff
	if !needAbs {
		needAbs = fs.iwthabs >= maxIWthAbs
		fs.iwthabs++
	}
	if needAbs {
		f.AbsLineInfo = append(f.AbsLineInfo, bytecode.AbsLineInfo{
			PC:   int32(pc),
			Line: int32(line),
		})
		linedif = absLineInfo
		fs.iwthabs = 1
	}
	f.LineInfo = append(f.LineInfo, int8(linedif))
	fs.previousline = line
}

func (fs *funcState) removeLastLineInfo() {
	f := fs.f
	pc := fs.pc - 1
	if f.LineInfo[pc] != absLineInfo {
		fs.previousline -= int(f.LineInfo[pc])
		fs.iwthabs--
	} else {
		f.AbsLineInfo = f.AbsLineInfo[:len(f.AbsLineInfo)-1]
		fs.iwthabs = maxIWthAbs + 1
	}
	f.LineInfo = f.LineInfo[:pc]
}

func (fs *funcState) removeLastInstruction() {
	fs.removeLastLineInfo()
	fs.pc--
	fs.f.Code = fs.f.Code[:fs.pc]
}

func (fs *funcState) fixLine(line int) {
	fs.removeLastLineInfo()
	fs.saveLineInfo(line)
}

func (fs *funcState) code(i bytecode.Instruction) int {
	fs.f.Code = append(fs.f.Code, i)
	fs.pc++
	fs.saveLineInfo(fs.ls.lastline)
	return fs.pc - 1
}

func (fs *funcState) codeABC(o bytecode.OpCode, a, b, c int) int {
	return fs.codeABCk(o, a, b, c, 0)
}

func (fs *funcState) codeABCk(o bytecode.OpCode, a, b, c, k int) int {
	return fs.code(createABCk(o, a, b, c, k))
}

func (fs *funcState) codevABCk(o bytecode.OpCode, a, b, c, k int) int {
	return fs.code(createvABCk(o, a, b, c, k))
}

func (fs *funcState) codeABx(o bytecode.OpCode, a, bx int) int {
	return fs.code(createABx(o, a, bx))
}

func (fs *funcState) codeAsBx(o bytecode.OpCode, a, sbx int) int {
	return fs.code(createABx(o, a, sbx+bytecode.OffsetSBx))
}

func (fs *funcState) codeSJ(o bytecode.OpCode, sj int) int {
	return fs.code(createSJ(o, sj+bytecode.OffsetSJ))
}

func (fs *funcState) codeExtraArg(a int) int {
	return fs.code(createAx(bytecode.OP_EXTRAARG, a))
}

func (fs *funcState) codeK(reg, k int) int {
	if k <= bytecode.MaxArgBx {
		return fs.codeABx(bytecode.OP_LOADK, reg, k)
	}
	p := fs.codeABx(bytecode.OP_LOADKX, reg, 0)
	fs.codeExtraArg(k)
	return p
}

func (fs *funcState) checkStack(n int) {
	newStack := int(fs.freereg) + n
	if newStack > int(fs.f.MaxStackSize) {
		fs.checkLimit(newStack, maxFStack, "registers")
		fs.f.MaxStackSize = uint8(newStack)
	}
}

func (fs *funcState) reserveRegs(n int) {
	fs.checkStack(n)
	fs.freereg = uint8(int(fs.freereg) + n)
}

func (fs *funcState) freeReg(reg int) {
	if reg >= int(fs.nVarStack()) {
		fs.freereg--
	}
}

func (fs *funcState) freeRegs(r1, r2 int) {
	if r1 > r2 {
		fs.freeReg(r1)
		fs.freeReg(r2)
	} else {
		fs.freeReg(r2)
		fs.freeReg(r1)
	}
}

func (fs *funcState) freeExp(e *expDesc) {
	if e.k == vNonReloc {
		fs.freeReg(e.info)
	}
}

func (fs *funcState) freeExps(e1, e2 *expDesc) {
	r1 := -1
	if e1.k == vNonReloc {
		r1 = e1.info
	}
	r2 := -1
	if e2.k == vNonReloc {
		r2 = e2.info
	}
	fs.freeRegs(r1, r2)
}

func (fs *funcState) addK(c bytecode.Constant) int {
	k := len(fs.f.Consts)
	fs.checkLimit(k+1, bytecode.MaxArgAx+1, "constants")
	fs.f.Consts = append(fs.f.Consts, c)
	return k
}

func (fs *funcState) stringK(s string) int {
	if idx, ok := fs.kStr[s]; ok {
		return idx
	}
	idx := fs.addK(bytecode.Constant{
		Kind:    bytecode.ConstString,
		Str:     s,
		IsShort: len(s) <= maxShortStrLen,
	})
	fs.kStr[s] = idx
	return idx
}

func (fs *funcState) intK(n int64) int {
	if idx, ok := fs.kInt[n]; ok {
		return idx
	}
	idx := fs.addK(bytecode.Constant{Kind: bytecode.ConstInt, Int: n})
	fs.kInt[n] = idx
	return idx
}

func (fs *funcState) numberK(r float64) int {
	if r == 0 {
		if fs.kZeroFlt >= 0 {
			return fs.kZeroFlt
		}
		fs.kZeroFlt = fs.addK(bytecode.Constant{Kind: bytecode.ConstFloat, Float: r})
		return fs.kZeroFlt
	}
	const q = 0x1p-52 // ldexp(1.0, -MANT_DIG + 1) for IEEE-754 float64
	k := r * (1 + q)
	if _, isInt := fltToIntEq(k); !isInt && !math.IsNaN(k) {
		bits := math.Float64bits(k)
		if idx, ok := fs.kFlt[bits]; ok {
			if fs.f.Consts[idx].Kind == bytecode.ConstFloat && fs.f.Consts[idx].Float == r {
				return idx
			}
			return fs.addK(bytecode.Constant{Kind: bytecode.ConstFloat, Float: r})
		}
		idx := fs.addK(bytecode.Constant{Kind: bytecode.ConstFloat, Float: r})
		fs.kFlt[bits] = idx
		return idx
	}
	return fs.addK(bytecode.Constant{Kind: bytecode.ConstFloat, Float: r})
}

func (fs *funcState) boolF() int {
	if fs.kFalse >= 0 {
		return fs.kFalse
	}
	fs.kFalse = fs.addK(bytecode.Constant{Kind: bytecode.ConstFalse})
	return fs.kFalse
}

func (fs *funcState) boolT() int {
	if fs.kTrue >= 0 {
		return fs.kTrue
	}
	fs.kTrue = fs.addK(bytecode.Constant{Kind: bytecode.ConstTrue})
	return fs.kTrue
}

func (fs *funcState) nilK() int {
	if fs.kNil >= 0 {
		return fs.kNil
	}
	fs.kNil = fs.addK(bytecode.Constant{Kind: bytecode.ConstNil})
	return fs.kNil
}

func (fs *funcState) codeInt(reg int, i int64) {
	if fitsBx(i) {
		fs.codeAsBx(bytecode.OP_LOADI, reg, int(i))
	} else {
		fs.codeK(reg, fs.intK(i))
	}
}

func (fs *funcState) codeFloat(reg int, f float64) {
	if fi, ok := fltToIntEq(f); ok && fitsBx(fi) {
		fs.codeAsBx(bytecode.OP_LOADF, reg, int(fi))
	} else {
		fs.codeK(reg, fs.numberK(f))
	}
}

func (fs *funcState) codeCheckGlobal(v *expDesc, k, line int) {
	fs.exp2AnyReg(v)
	fs.fixLine(line)
	if k >= bytecode.MaxArgBx {
		k = 0
	} else {
		k++
	}
	fs.codeABx(bytecode.OP_ERRNNIL, v.info, k)
	fs.fixLine(line)
	fs.freeExp(v)
}

func (fs *funcState) setReturns(e *expDesc, nresults int) {
	fs.checkLimit(nresults+1, bytecode.MaxArgC, "multiple results")
	pc := &fs.f.Code[e.info]
	if e.k == vCall {
		setArgC(pc, nresults+1)
	} else {
		// vVararg
		setArgC(pc, nresults+1)
		setArgA(pc, int(fs.freereg))
		fs.reserveRegs(1)
	}
}

func (fs *funcState) setMultRet(e *expDesc) {
	fs.setReturns(e, luaMultRet)
}

func (fs *funcState) str2K(e *expDesc) int {
	e.info = fs.stringK(e.strval)
	e.k = vK
	return e.info
}

func (fs *funcState) setOneRet(e *expDesc) {
	if e.k == vCall {
		e.k = vNonReloc
		e.info = fs.f.Code[e.info].A()
	} else if e.k == vVararg {
		setArgC(&fs.f.Code[e.info], 2)
		e.k = vReloc
	}
}

func (fs *funcState) needVaTab() {
	fs.f.Flag |= bytecode.PFVarargTable
}

func (fs *funcState) vapar2Local(v *expDesc) {
	fs.needVaTab()
	v.k = vLocal
}

func (fs *funcState) dischargeVars(e *expDesc) {
	switch e.k {
	case vConst:
		const2Exp(fs.const2Val(e), e)
	case vVargVar:
		fs.vapar2Local(e)
		fallthrough
	case vLocal:
		e.info = int(e.varInfo.ridx)
		e.k = vNonReloc
	case vUpval:
		e.info = fs.codeABC(bytecode.OP_GETUPVAL, 0, e.info, 0)
		e.k = vReloc
	case vIndexUp:
		e.info = fs.codeABC(bytecode.OP_GETTABUP, 0, int(e.ind.t), int(e.ind.idx))
		e.k = vReloc
	case vIndexI:
		fs.freeReg(int(e.ind.t))
		e.info = fs.codeABC(bytecode.OP_GETI, 0, int(e.ind.t), int(e.ind.idx))
		e.k = vReloc
	case vIndexStr:
		fs.freeReg(int(e.ind.t))
		e.info = fs.codeABC(bytecode.OP_GETFIELD, 0, int(e.ind.t), int(e.ind.idx))
		e.k = vReloc
	case vIndexed:
		fs.freeRegs(int(e.ind.t), int(e.ind.idx))
		e.info = fs.codeABC(bytecode.OP_GETTABLE, 0, int(e.ind.t), int(e.ind.idx))
		e.k = vReloc
	case vVargInd:
		fs.freeRegs(int(e.ind.t), int(e.ind.idx))
		e.info = fs.codeABC(bytecode.OP_GETVARG, 0, int(e.ind.t), int(e.ind.idx))
		e.k = vReloc
	case vVararg, vCall:
		fs.setOneRet(e)
	}
}

func (fs *funcState) discharge2Reg(e *expDesc, reg int) {
	fs.dischargeVars(e)
	switch e.k {
	case vNil:
		fs.codeNil(reg, 1)
	case vFalse:
		fs.codeABC(bytecode.OP_LOADFALSE, reg, 0, 0)
	case vTrue:
		fs.codeABC(bytecode.OP_LOADTRUE, reg, 0, 0)
	case vKStr:
		fs.str2K(e)
		fallthrough
	case vK:
		fs.codeK(reg, e.info)
	case vKFlt:
		fs.codeFloat(reg, e.nval)
	case vKInt:
		fs.codeInt(reg, e.ival)
	case vReloc:
		setArgA(&fs.f.Code[e.info], reg)
	case vNonReloc:
		if reg != e.info {
			fs.codeABC(bytecode.OP_MOVE, reg, e.info, 0)
		}
	default:
		// vJmp
		return
	}
	e.info = reg
	e.k = vNonReloc
}

func (fs *funcState) discharge2AnyReg(e *expDesc) {
	if e.k != vNonReloc {
		fs.reserveRegs(1)
		fs.discharge2Reg(e, int(fs.freereg)-1)
	}
}

func (fs *funcState) codeLoadBool(a int, op bytecode.OpCode) int {
	fs.getLabel()
	return fs.codeABC(op, a, 0, 0)
}

func (fs *funcState) needValue(list int) bool {
	for list != noJump {
		inst := fs.f.Code[fs.getJumpControlPC(list)]
		if inst.Op() != bytecode.OP_TESTSET {
			return true
		}
		list = fs.getJump(list)
	}
	return false
}

func (fs *funcState) exp2Reg(e *expDesc, reg int) {
	fs.discharge2Reg(e, reg)
	if e.k == vJmp {
		fs.concat(&e.t, e.info)
	}
	if e.hasJumps() {
		pF := noJump
		pT := noJump
		if fs.needValue(e.t) || fs.needValue(e.f) {
			fj := noJump
			if e.k != vJmp {
				fj = fs.jump()
			}
			pF = fs.codeLoadBool(reg, bytecode.OP_LFALSESKIP)
			pT = fs.codeLoadBool(reg, bytecode.OP_LOADTRUE)
			fs.patchToHere(fj)
		}
		final := fs.getLabel()
		fs.patchListAux(e.f, final, reg, pF)
		fs.patchListAux(e.t, final, reg, pT)
	}
	e.f = noJump
	e.t = noJump
	e.info = reg
	e.k = vNonReloc
}

func (fs *funcState) exp2NextReg(e *expDesc) {
	fs.dischargeVars(e)
	fs.freeExp(e)
	fs.reserveRegs(1)
	fs.exp2Reg(e, int(fs.freereg)-1)
}

func (fs *funcState) exp2AnyReg(e *expDesc) int {
	fs.dischargeVars(e)
	if e.k == vNonReloc {
		if !e.hasJumps() {
			return e.info
		}
		if e.info >= int(fs.nVarStack()) {
			fs.exp2Reg(e, e.info)
			return e.info
		}
	}
	fs.exp2NextReg(e)
	return e.info
}

func (fs *funcState) exp2AnyRegUp(e *expDesc) {
	if (e.k != vUpval && e.k != vVargVar) || e.hasJumps() {
		fs.exp2AnyReg(e)
	}
}

func (fs *funcState) exp2Val(e *expDesc) {
	if e.k == vJmp || e.hasJumps() {
		fs.exp2AnyReg(e)
	} else {
		fs.dischargeVars(e)
	}
}

func (fs *funcState) exp2K(e *expDesc) bool {
	if !e.hasJumps() {
		var info int
		switch e.k {
		case vTrue:
			info = fs.boolT()
		case vFalse:
			info = fs.boolF()
		case vNil:
			info = fs.nilK()
		case vKInt:
			info = fs.intK(e.ival)
		case vKFlt:
			info = fs.numberK(e.nval)
		case vKStr:
			info = fs.stringK(e.strval)
		case vK:
			info = e.info
		default:
			return false
		}
		if info <= maxIndexRK {
			e.k = vK
			e.info = info
			return true
		}
	}
	return false
}

func (fs *funcState) exp2RK(e *expDesc) int {
	if fs.exp2K(e) {
		return 1
	}
	fs.exp2AnyReg(e)
	return 0
}

func (fs *funcState) codeABRK(o bytecode.OpCode, a, b int, ec *expDesc) {
	k := fs.exp2RK(ec)
	fs.codeABCk(o, a, b, ec.info, k)
}

func (fs *funcState) storeVar(v, ex *expDesc) {
	switch v.k {
	case vLocal:
		fs.freeExp(ex)
		fs.exp2Reg(ex, int(v.varInfo.ridx))
		return
	case vUpval:
		e := fs.exp2AnyReg(ex)
		fs.codeABC(bytecode.OP_SETUPVAL, e, v.info, 0)
	case vIndexUp:
		fs.codeABRK(bytecode.OP_SETTABUP, int(v.ind.t), int(v.ind.idx), ex)
	case vIndexI:
		fs.codeABRK(bytecode.OP_SETI, int(v.ind.t), int(v.ind.idx), ex)
	case vIndexStr:
		fs.codeABRK(bytecode.OP_SETFIELD, int(v.ind.t), int(v.ind.idx), ex)
	case vVargInd:
		fs.needVaTab()
		fallthrough
	case vIndexed:
		fs.codeABRK(bytecode.OP_SETTABLE, int(v.ind.t), int(v.ind.idx), ex)
	default:
		panic("invalid var kind to store")
	}
	fs.freeExp(ex)
}

func (fs *funcState) negateCondition(e *expDesc) {
	inst := &fs.f.Code[fs.getJumpControlPC(e.info)]
	setArgK(inst, inst.KBit()^1)
}

func (fs *funcState) jumpOnCond(e *expDesc, cond int) int {
	if e.k == vReloc {
		ie := fs.f.Code[e.info]
		if ie.Op() == bytecode.OP_NOT {
			fs.removeLastInstruction()
			notCond := 0
			if cond == 0 {
				notCond = 1
			}
			return fs.condJump(bytecode.OP_TEST, ie.B(), 0, 0, notCond)
		}
	}
	fs.discharge2AnyReg(e)
	fs.freeExp(e)
	return fs.condJump(bytecode.OP_TESTSET, noReg, e.info, 0, cond)
}

func (fs *funcState) goIfTrue(e *expDesc) {
	var pc int
	fs.dischargeVars(e)
	switch e.k {
	case vJmp:
		fs.negateCondition(e)
		pc = e.info
	case vK, vKFlt, vKInt, vKStr, vTrue:
		pc = noJump
	default:
		pc = fs.jumpOnCond(e, 0)
	}
	fs.concat(&e.f, pc)
	fs.patchToHere(e.t)
	e.t = noJump
}

func (fs *funcState) goIfFalse(e *expDesc) {
	var pc int
	fs.dischargeVars(e)
	switch e.k {
	case vJmp:
		pc = e.info
	case vNil, vFalse:
		pc = noJump
	default:
		pc = fs.jumpOnCond(e, 1)
	}
	fs.concat(&e.t, pc)
	fs.patchToHere(e.f)
	e.f = noJump
}

func (fs *funcState) codeNot(e *expDesc) {
	switch e.k {
	case vNil, vFalse:
		e.k = vTrue
	case vK, vKFlt, vKInt, vKStr, vTrue:
		e.k = vFalse
	case vJmp:
		fs.negateCondition(e)
	case vReloc, vNonReloc:
		fs.discharge2AnyReg(e)
		fs.freeExp(e)
		e.info = fs.codeABC(bytecode.OP_NOT, 0, e.info, 0)
		e.k = vReloc
	default:
		panic("unreachable codeNot")
	}
	e.f, e.t = e.t, e.f
	fs.removeValues(e.f)
	fs.removeValues(e.t)
}

func (fs *funcState) isKStr(e *expDesc) bool {
	if e.k == vK && !e.hasJumps() && e.info <= maxIndexRK {
		c := fs.f.Consts[e.info]
		return c.Kind == bytecode.ConstString && c.IsShort
	}
	return false
}

func isKInt(e *expDesc) bool {
	return e.k == vKInt && !e.hasJumps()
}

func isCInt(e *expDesc) bool {
	return isKInt(e) && uint64(e.ival) <= uint64(bytecode.MaxArgC)
}

func isSCInt(e *expDesc) bool {
	return isKInt(e) && fitsC(e.ival)
}

func isSCNumber(e *expDesc, pi *int, isFloat *int) bool {
	var i int64
	if e.k == vKInt {
		i = e.ival
	} else if e.k == vKFlt {
		var good bool
		i, good = fltToIntEq(e.nval)
		if !good {
			return false
		}
		*isFloat = 1
	} else {
		return false
	}
	if !e.hasJumps() && fitsC(i) {
		*pi = int2sC(int(i))
		return true
	}
	return false
}

func (fs *funcState) codeSelf(e, key *expDesc) {
	fs.exp2AnyReg(e)
	ereg := e.info
	fs.freeExp(e)
	base := int(fs.freereg)
	e.info = base
	e.k = vNonReloc
	fs.reserveRegs(2)
	if len(key.strval) <= maxShortStrLen && fs.exp2K(key) {
		fs.codeABCk(bytecode.OP_SELF, base, ereg, key.info, 0)
	} else {
		fs.exp2AnyReg(key)
		fs.codeABC(bytecode.OP_MOVE, base+1, ereg, 0)
		fs.codeABC(bytecode.OP_GETTABLE, base, ereg, key.info)
	}
	fs.freeExp(key)
}

func fillIdxK(t *expDesc, idx int, k expKind) {
	t.ind.idx = int16(uint8(idx))
	t.k = k
}

func (fs *funcState) indexed(t, k *expDesc) {
	keystr := -1
	if k.k == vKStr {
		keystr = fs.str2K(k)
	}
	if t.k == vUpval && !fs.isKStr(k) {
		fs.exp2AnyReg(t)
	}
	if t.k == vUpval {
		t.ind.t = uint8(t.info)
		fillIdxK(t, k.info, vIndexUp)
	} else if t.k == vVargVar {
		kreg := fs.exp2AnyReg(k)
		t.ind.t = t.varInfo.ridx
		fillIdxK(t, kreg, vVargInd)
	} else {
		if t.k == vLocal {
			t.ind.t = t.varInfo.ridx
		} else {
			t.ind.t = uint8(t.info)
		}
		if fs.isKStr(k) {
			fillIdxK(t, k.info, vIndexStr)
		} else if isCInt(k) {
			fillIdxK(t, int(k.ival), vIndexI)
		} else {
			fillIdxK(t, fs.exp2AnyReg(k), vIndexed)
		}
	}
	t.ind.keystr = keystr
	t.ind.ro = 0
}

func constFolding(op int, e1, e2 *expDesc) bool {
	v1, ok1 := toNumeral(e1)
	v2, ok2 := toNumeral(e2)
	if !ok1 || !ok2 || !validFoldOp(op, v1, v2) {
		return false
	}
	res, ok := rawArith(op, v1, v2)
	if !ok {
		return false
	}
	if res.Kind == bytecode.ConstInt {
		e1.k = vKInt
		e1.ival = res.Int
	} else {
		if math.IsNaN(res.Float) || res.Float == 0 {
			return false
		}
		e1.k = vKFlt
		e1.nval = res.Float
	}
	return true
}

func (fs *funcState) codeUnExpVal(op bytecode.OpCode, e *expDesc, line int) {
	r := fs.exp2AnyReg(e)
	fs.freeExp(e)
	e.info = fs.codeABC(op, 0, r, 0)
	e.k = vReloc
	fs.fixLine(line)
}

func (fs *funcState) finishBinExpVal(e1, e2 *expDesc, op bytecode.OpCode, v2, flip, line int, mmop bytecode.OpCode, event tmEvent) {
	v1 := fs.exp2AnyReg(e1)
	pc := fs.codeABCk(op, 0, v1, v2, 0)
	fs.freeExps(e1, e2)
	e1.info = pc
	e1.k = vReloc
	fs.fixLine(line)
	fs.codeABCk(mmop, v1, v2, int(event), flip)
	fs.fixLine(line)
}

func (fs *funcState) codeBinExpVal(opr binOpr, e1, e2 *expDesc, line int) {
	op := binOpr2Op(opr, oprAdd, bytecode.OP_ADD)
	v2 := fs.exp2AnyReg(e2)
	fs.finishBinExpVal(e1, e2, op, v2, 0, line, bytecode.OP_MMBIN, binOpr2TM(opr))
}

func (fs *funcState) codeBinI(op bytecode.OpCode, e1, e2 *expDesc, flip, line int, event tmEvent) {
	v2 := int2sC(int(e2.ival))
	fs.finishBinExpVal(e1, e2, op, v2, flip, line, bytecode.OP_MMBINI, event)
}

func (fs *funcState) codeBinK(opr binOpr, e1, e2 *expDesc, flip, line int) {
	event := binOpr2TM(opr)
	v2 := e2.info
	op := binOpr2Op(opr, oprAdd, bytecode.OP_ADDK)
	fs.finishBinExpVal(e1, e2, op, v2, flip, line, bytecode.OP_MMBINK, event)
}

func (fs *funcState) finishBinExpNeg(e1, e2 *expDesc, op bytecode.OpCode, line int, event tmEvent) bool {
	if !isKInt(e2) {
		return false
	}
	i2 := e2.ival
	if !(fitsC(i2) && fitsC(-i2)) {
		return false
	}
	v2 := int(i2)
	fs.finishBinExpVal(e1, e2, op, int2sC(-v2), 0, line, bytecode.OP_MMBINI, event)
	setArgB(&fs.f.Code[fs.pc-1], int2sC(v2))
	return true
}

func swapExps(e1, e2 *expDesc) {
	*e1, *e2 = *e2, *e1
}

func (fs *funcState) codeBinNoK(opr binOpr, e1, e2 *expDesc, flip, line int) {
	if flip != 0 {
		swapExps(e1, e2)
	}
	fs.codeBinExpVal(opr, e1, e2, line)
}

func (fs *funcState) codeArith(opr binOpr, e1, e2 *expDesc, flip, line int) {
	if _, ok := toNumeral(e2); ok && fs.exp2K(e2) {
		fs.codeBinK(opr, e1, e2, flip, line)
	} else {
		fs.codeBinNoK(opr, e1, e2, flip, line)
	}
}

func (fs *funcState) codeCommutative(op binOpr, e1, e2 *expDesc, line int) {
	flip := 0
	if _, ok := toNumeral(e1); ok {
		swapExps(e1, e2)
		flip = 1
	}
	if op == oprAdd && isSCInt(e2) {
		fs.codeBinI(bytecode.OP_ADDI, e1, e2, flip, line, tmAdd)
	} else {
		fs.codeArith(op, e1, e2, flip, line)
	}
}

func (fs *funcState) codeBitwise(opr binOpr, e1, e2 *expDesc, line int) {
	flip := 0
	if e1.k == vKInt {
		swapExps(e1, e2)
		flip = 1
	}
	if e2.k == vKInt && fs.exp2K(e2) {
		fs.codeBinK(opr, e1, e2, flip, line)
	} else {
		fs.codeBinNoK(opr, e1, e2, flip, line)
	}
}

func (fs *funcState) codeOrder(opr binOpr, e1, e2 *expDesc) {
	var r1, r2, im, isFloat int
	var op bytecode.OpCode
	if isSCNumber(e2, &im, &isFloat) {
		r1 = fs.exp2AnyReg(e1)
		r2 = im
		op = binOpr2Op(opr, oprLT, bytecode.OP_LTI)
	} else if isSCNumber(e1, &im, &isFloat) {
		r1 = fs.exp2AnyReg(e2)
		r2 = im
		op = binOpr2Op(opr, oprLT, bytecode.OP_GTI)
	} else {
		r1 = fs.exp2AnyReg(e1)
		r2 = fs.exp2AnyReg(e2)
		op = binOpr2Op(opr, oprLT, bytecode.OP_LT)
	}
	fs.freeExps(e1, e2)
	e1.info = fs.condJump(op, r1, r2, isFloat, 1)
	e1.k = vJmp
}

func (fs *funcState) codeEQ(opr binOpr, e1, e2 *expDesc) {
	var r1, r2, im, isFloat int
	var op bytecode.OpCode
	if e1.k != vNonReloc {
		swapExps(e1, e2)
	}
	r1 = fs.exp2AnyReg(e1)
	if isSCNumber(e2, &im, &isFloat) {
		op = bytecode.OP_EQI
		r2 = im
	} else if fs.exp2RK(e2) != 0 {
		op = bytecode.OP_EQK
		r2 = e2.info
	} else {
		op = bytecode.OP_EQ
		r2 = fs.exp2AnyReg(e2)
	}
	fs.freeExps(e1, e2)
	eqFlag := 0
	if opr == oprEQ {
		eqFlag = 1
	}
	e1.info = fs.condJump(op, r1, r2, isFloat, eqFlag)
	e1.k = vJmp
}

func (fs *funcState) prefix(opr unOpr, e *expDesc, line int) {
	ef := expDesc{k: vKInt, t: noJump, f: noJump}
	fs.dischargeVars(e)
	switch opr {
	case oprMinus, oprBNot:
		if constFolding(int(opr)+luaOpUnm, e, &ef) {
			return
		}
		fs.codeUnExpVal(unOpr2Op(opr), e, line)
	case oprLen:
		fs.codeUnExpVal(unOpr2Op(opr), e, line)
	case oprNot:
		fs.codeNot(e)
	default:
		panic("unreachable prefix op")
	}
}

func (fs *funcState) infix(op binOpr, v *expDesc) {
	fs.dischargeVars(v)
	switch op {
	case oprAnd:
		fs.goIfTrue(v)
	case oprOr:
		fs.goIfFalse(v)
	case oprConcat:
		fs.exp2NextReg(v)
	case oprAdd, oprSub, oprMul, oprDiv, oprIDiv, oprMod, oprPow,
		oprBAnd, oprBOr, oprBXor, oprShl, oprShr:
		if _, ok := toNumeral(v); !ok {
			fs.exp2AnyReg(v)
		}
	case oprEQ, oprNE:
		if _, ok := toNumeral(v); !ok {
			fs.exp2RK(v)
		}
	case oprLT, oprLE, oprGT, oprGE:
		var dummy, dummy2 int
		if !isSCNumber(v, &dummy, &dummy2) {
			fs.exp2AnyReg(v)
		}
	default:
		panic("unreachable infix op")
	}
}

func (fs *funcState) codeConcat(e1, e2 *expDesc, line int) {
	if fs.hasPreviousInst() {
		ie2 := &fs.f.Code[fs.pc-1]
		if ie2.Op() == bytecode.OP_CONCAT {
			n := ie2.B()
			fs.freeExp(e2)
			setArgA(ie2, e1.info)
			setArgB(ie2, n+1)
			return
		}
	}
	fs.codeABC(bytecode.OP_CONCAT, e1.info, 2, 0)
	fs.freeExp(e2)
	fs.fixLine(line)
}

func (fs *funcState) posfix(opr binOpr, e1, e2 *expDesc, line int) {
	fs.dischargeVars(e2)
	if opr.foldable() && constFolding(int(opr)+luaOpAdd, e1, e2) {
		return
	}
	switch opr {
	case oprAnd:
		fs.concat(&e2.f, e1.f)
		*e1 = *e2
	case oprOr:
		fs.concat(&e2.t, e1.t)
		*e1 = *e2
	case oprConcat:
		fs.exp2NextReg(e2)
		fs.codeConcat(e1, e2, line)
	case oprAdd, oprMul:
		fs.codeCommutative(opr, e1, e2, line)
	case oprSub:
		if fs.finishBinExpNeg(e1, e2, bytecode.OP_ADDI, line, tmSub) {
			return
		}
		fs.codeArith(opr, e1, e2, 0, line)
	case oprDiv, oprIDiv, oprMod, oprPow:
		fs.codeArith(opr, e1, e2, 0, line)
	case oprBAnd, oprBOr, oprBXor:
		fs.codeBitwise(opr, e1, e2, line)
	case oprShl:
		if isSCInt(e1) {
			swapExps(e1, e2)
			fs.codeBinI(bytecode.OP_SHLI, e1, e2, 1, line, tmShl)
		} else if !fs.finishBinExpNeg(e1, e2, bytecode.OP_SHRI, line, tmShl) {
			fs.codeBinExpVal(opr, e1, e2, line)
		}
	case oprShr:
		if isSCInt(e2) {
			fs.codeBinI(bytecode.OP_SHRI, e1, e2, 0, line, tmShr)
		} else {
			fs.codeBinExpVal(opr, e1, e2, line)
		}
	case oprEQ, oprNE:
		fs.codeEQ(opr, e1, e2)
	case oprGT, oprGE:
		swapExps(e1, e2)
		opr = binOpr(int(opr)-int(oprGT)) + oprLT
		fallthrough
	case oprLT, oprLE:
		fs.codeOrder(opr, e1, e2)
	default:
		panic("unreachable posfix op")
	}
}

func (fs *funcState) setTableSize(pc, ra, asize, hsize int) {
	extra := asize / (bytecode.MaxArgvC + 1)
	rc := asize % (bytecode.MaxArgvC + 1)
	k := 0
	if extra > 0 {
		k = 1
	}
	vb := 0
	if hsize != 0 {
		vb = int(ceilLog2(uint32(hsize))) + 1
	}
	fs.f.Code[pc] = createvABCk(bytecode.OP_NEWTABLE, ra, vb, rc, k)
	fs.f.Code[pc+1] = createAx(bytecode.OP_EXTRAARG, extra)
}

func (fs *funcState) setList(base, nelems, tostore int) {
	if tostore == luaMultRet {
		tostore = 0
	}
	if nelems <= bytecode.MaxArgvC {
		fs.codevABCk(bytecode.OP_SETLIST, base, tostore, nelems, 0)
	} else {
		extra := nelems / (bytecode.MaxArgvC + 1)
		nelems %= (bytecode.MaxArgvC + 1)
		fs.codevABCk(bytecode.OP_SETLIST, base, tostore, nelems, 1)
		fs.codeExtraArg(extra)
	}
	fs.freereg = uint8(base + 1)
}

func finalTarget(code []bytecode.Instruction, i int) int {
	for range 100 {
		pc := code[i]
		if pc.Op() != bytecode.OP_JMP {
			break
		}
		i += pc.SJ() + 1
	}
	return i
}

func (fs *funcState) finish() {
	p := fs.f
	if p.Flag&bytecode.PFVarargTable != 0 {
		p.Flag &^= bytecode.PFVarargHidden
	}
	for i := 0; i < fs.pc; i++ {
		pc := &p.Code[i]
		switch pc.Op() {
		case bytecode.OP_RETURN0, bytecode.OP_RETURN1:
			if !(fs.needclose || (p.Flag&bytecode.PFVarargHidden) != 0) {
				break
			}
			setOp(pc, bytecode.OP_RETURN)
			fallthrough
		case bytecode.OP_RETURN, bytecode.OP_TAILCALL:
			if fs.needclose {
				setArgK(pc, 1)
			}
			if p.Flag&bytecode.PFVarargHidden != 0 {
				setArgC(pc, int(p.NumParams)+1)
			}
		case bytecode.OP_GETVARG:
			if p.Flag&bytecode.PFVarargTable != 0 {
				setOp(pc, bytecode.OP_GETTABLE)
			}
		case bytecode.OP_VARARG:
			if p.Flag&bytecode.PFVarargTable != 0 {
				setArgK(pc, 1)
			}
		case bytecode.OP_JMP:
			target := finalTarget(p.Code, i)
			fs.fixJump(i, target)
		}
	}
}
