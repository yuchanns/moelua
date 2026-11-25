package bytecode

import "fmt"

// Verify validates a Proto tree before execution so unchecked VM fast paths
// cannot dereference out-of-bounds constants, upvalues, prototypes, or code offsets.
// It proves that:
//   - proto nesting depth is bounded (<= maxProtoDepth, preventing Go stack overflow)
//   - all opcodes are valid (< NumOpcodes) and EXTRAARG / MMBIN* pairings are well-formed
//   - all control-flow successors (fallthrough, skip-1, jumps, loops) lie inside [0, len(Code))
//     and never land on OP_EXTRAARG or an unpaired OP_MMBIN*
//   - all constant / short-string / upvalue / sub-proto / TM / register indices lie in bounds
//   - OP_VARARGPREP appears at pc 0 iff p.IsVararg(), and OP_GETVARG requires PFVarargHidden
//   - MaxStackSize fits in the 256-slot register window
//
// Binary chunks remain trusted host/application input; Verify provides panic-freedom
// defense in depth rather than a security sandbox boundary.
func Verify(p *Proto) error {
	return VerifyWithControls(p, nil, nil)
}

// VerifyWithControls validates a Proto tree while honoring optional incremental
// memory-budget and cooperative cancellation callbacks.
func VerifyWithControls(p *Proto, checkMem func(int64) error, cancelFn func() error) error {
	var memUsed int64
	var work int64
	return verifyDepth(p, 0, checkMem, &memUsed, cancelFn, &work)
}

func verifyTick(cancelFn func() error, work *int64, delta int64) error {
	if cancelFn == nil || delta <= 0 {
		return nil
	}
	prev := *work >> 12
	*work += delta
	next := *work >> 12
	for prev < next {
		if err := cancelFn(); err != nil {
			return err
		}
		prev++
	}
	return nil
}

func verifyDepth(p *Proto, depth int, checkMem func(int64) error, memUsed *int64, cancelFn func() error, work *int64) error {
	if p == nil {
		return fmt.Errorf("verify: nil proto")
	}
	if depth > maxProtoDepth {
		return fmt.Errorf("verify: proto nesting depth exceeds limit (%d)", maxProtoDepth)
	}
	if err := verifyTick(cancelFn, work, 64); err != nil {
		return err
	}
	if len(p.Code) == 0 {
		return fmt.Errorf("verify: empty code in proto %q", p.Source)
	}
	if len(p.Upvalues) > MaxArgA {
		return fmt.Errorf("verify: too many upvalues %d > %d", len(p.Upvalues), MaxArgA)
	}
	if p.Flag&(PFVarargHidden|PFVarargTable) == (PFVarargHidden | PFVarargTable) {
		return fmt.Errorf("verify: conflicting vararg flags 0x%02x", p.Flag)
	}
	if p.Flag &^ (PFVarargHidden|PFVarargTable|PFFixed) != 0 {
		return fmt.Errorf("verify: invalid proto flags 0x%02x", p.Flag)
	}
	if p.MaxStackSize > MaxArgA {
		return fmt.Errorf("verify: maxstacksize %d > %d", p.MaxStackSize, MaxArgA)
	}
	if int(p.NumParams) > int(p.MaxStackSize) {
		return fmt.Errorf("verify: numparams %d > maxstacksize %d", p.NumParams, p.MaxStackSize)
	}
	if len(p.LineInfo) != 0 && len(p.LineInfo) != len(p.Code) {
		return fmt.Errorf("verify: lineinfo len %d != code len %d", len(p.LineInfo), len(p.Code))
	}
	if len(p.AbsLineInfo) > 0 {
		if len(p.LineInfo) == 0 {
			return fmt.Errorf("verify: abslineinfo present without lineinfo")
		}
		for i, ali := range p.AbsLineInfo {
			if i > 0 && (i&4095) == 0 {
				if err := verifyTick(cancelFn, work, 4096); err != nil {
					return err
				}
			}
			if ali.PC < 0 || int(ali.PC) >= len(p.LineInfo) {
				return fmt.Errorf("verify: abslineinfo[%d].PC=%d out of [0,%d)", i, ali.PC, len(p.LineInfo))
			}
			if i > 0 && ali.PC <= p.AbsLineInfo[i-1].PC {
				return fmt.Errorf("verify: abslineinfo[%d].PC=%d not strictly increasing after %d", i, ali.PC, p.AbsLineInfo[i-1].PC)
			}
		}
	}

	n := len(p.Code)
	if checkMem != nil {
		scratchBytes := int64(n)*6 + int64(len(p.Protos))
		if err := checkMem(*memUsed + scratchBytes); err != nil {
			return err
		}
		*memUsed += int64(n) * 4 // ICSlot is retained on Proto
	}
	maxStack := int(p.MaxStackSize)
	p.ICSlot = make([]int32, n)
	for i := range p.ICSlot {
		p.ICSlot[i] = -1
	}
	isExtraArg := make([]bool, n)
	isMMBin := make([]bool, n)
	verifiedClosure := make([]bool, len(p.Protos))
	icCount := 0

	// M4: OP_VARARGPREP must appear at pc 0 iff proto is vararg.
	if p.IsVararg() {
		if p.Code[0].Op() != OP_VARARGPREP {
			return fmt.Errorf("verify: vararg proto missing OP_VARARGPREP at pc 0")
		}
	} else if p.Code[0].Op() == OP_VARARGPREP {
		return fmt.Errorf("verify: non-vararg proto has OP_VARARGPREP at pc 0")
	}

	checkReg := func(pc int, op OpCode, field string, reg int) error {
		if reg >= maxStack {
			return fmt.Errorf("verify: pc %d %s %s=%d >= maxstack %d", pc, OpNames[op], field, reg, maxStack)
		}
		return nil
	}

	checkShortStrConst := func(pc int, op OpCode, field string, idx int) error {
		if idx >= len(p.Consts) || p.Consts[idx].Kind != ConstString {
			return fmt.Errorf("verify: pc %d %s %s=%d not string const", pc, OpNames[op], field, idx)
		}
		if len(p.Consts[idx].Str) > maxShortLen {
			return fmt.Errorf("verify: pc %d %s %s=%d string len %d > %d", pc, OpNames[op], field, idx, len(p.Consts[idx].Str), maxShortLen)
		}
		return nil
	}

	for pc := 0; pc < n; pc++ {
		if cancelFn != nil && pc > 0 && (pc&4095) == 0 {
			if err := verifyTick(cancelFn, work, 4096); err != nil {
				return err
			}
		}
		inst := p.Code[pc]
		op := inst.Op()
		if op >= NumOpcodes {
			return fmt.Errorf("verify: pc %d invalid opcode %d", pc, op)
		}
		if op == OP_EXTRAARG {
			return fmt.Errorf("verify: pc %d standalone EXTRAARG", pc)
		}
		switch op {
		case OP_MOVE, OP_UNM, OP_BNOT, OP_NOT, OP_LEN:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "B", inst.B()); err != nil {
				return err
			}
		case OP_LOADI, OP_LOADF, OP_LOADFALSE, OP_LOADTRUE, OP_CLOSE, OP_TBC, OP_RETURN1:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
		case OP_LOADK:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if inst.Bx() >= len(p.Consts) {
				return fmt.Errorf("verify: pc %d LOADK Bx=%d >= %d", pc, inst.Bx(), len(p.Consts))
			}
		case OP_LOADKX:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if pc+1 >= n || p.Code[pc+1].Op() != OP_EXTRAARG {
				return fmt.Errorf("verify: pc %d LOADKX missing EXTRAARG", pc)
			}
			ax := p.Code[pc+1].Ax()
			if ax >= len(p.Consts) {
				return fmt.Errorf("verify: pc %d LOADKX Ax=%d >= %d", pc, ax, len(p.Consts))
			}
			isExtraArg[pc+1] = true
			pc++
		case OP_LFALSESKIP:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			// M1: OP_LFALSESKIP skips pc+1 and lands on pc+2!
			if pc+2 >= n {
				return fmt.Errorf("verify: pc %d LFALSESKIP skips past end of code (%d)", pc, n)
			}
		case OP_LOADNIL:
			if inst.A()+inst.B() >= maxStack {
				return fmt.Errorf("verify: pc %d LOADNIL range %d..%d >= maxstack %d", pc, inst.A(), inst.A()+inst.B(), maxStack)
			}
		case OP_GETUPVAL, OP_SETUPVAL:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if inst.B() >= len(p.Upvalues) {
				return fmt.Errorf("verify: pc %d upval B=%d >= %d", pc, inst.B(), len(p.Upvalues))
			}
		case OP_GETTABUP:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if inst.B() >= len(p.Upvalues) {
				return fmt.Errorf("verify: pc %d GETTABUP B=%d >= %d", pc, inst.B(), len(p.Upvalues))
			}
			if err := checkShortStrConst(pc, op, "C", inst.C()); err != nil {
				return err
			}
			p.ICSlot[pc] = int32(icCount)
			icCount++
		case OP_SETTABUP:
			if inst.A() >= len(p.Upvalues) {
				return fmt.Errorf("verify: pc %d SETTABUP A=%d >= %d", pc, inst.A(), len(p.Upvalues))
			}
			if err := checkShortStrConst(pc, op, "B", inst.B()); err != nil {
				return err
			}
			if inst.K() {
				if inst.C() >= len(p.Consts) {
					return fmt.Errorf("verify: pc %d SETTABUP K C=%d >= %d", pc, inst.C(), len(p.Consts))
				}
			} else if err := checkReg(pc, op, "C", inst.C()); err != nil {
				return err
			}
		case OP_GETTABLE:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "B", inst.B()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "C", inst.C()); err != nil {
				return err
			}
		case OP_GETI:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "B", inst.B()); err != nil {
				return err
			}
		case OP_GETFIELD, OP_SELF:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "B", inst.B()); err != nil {
				return err
			}
			if op == OP_SELF && inst.A()+1 >= maxStack {
				return fmt.Errorf("verify: pc %d SELF A=%d out of maxstack %d", pc, inst.A(), maxStack)
			}
			if err := checkShortStrConst(pc, op, "C", inst.C()); err != nil {
				return err
			}
			p.ICSlot[pc] = int32(icCount)
			icCount++
		case OP_SETFIELD:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkShortStrConst(pc, op, "B", inst.B()); err != nil {
				return err
			}
			if inst.K() {
				if inst.C() >= len(p.Consts) {
					return fmt.Errorf("verify: pc %d SETFIELD K C=%d >= %d", pc, inst.C(), len(p.Consts))
				}
			} else if err := checkReg(pc, op, "C", inst.C()); err != nil {
				return err
			}
			p.ICSlot[pc] = int32(icCount)
			icCount++
		case OP_SETTABLE:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "B", inst.B()); err != nil {
				return err
			}
			if inst.K() {
				if inst.C() >= len(p.Consts) {
					return fmt.Errorf("verify: pc %d SETTABLE K C=%d >= %d", pc, inst.C(), len(p.Consts))
				}
			} else if err := checkReg(pc, op, "C", inst.C()); err != nil {
				return err
			}
		case OP_SETI:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if inst.K() {
				if inst.C() >= len(p.Consts) {
					return fmt.Errorf("verify: pc %d SETI K C=%d >= %d", pc, inst.C(), len(p.Consts))
				}
			} else if err := checkReg(pc, op, "C", inst.C()); err != nil {
				return err
			}
		case OP_NEWTABLE:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if pc+1 >= n || p.Code[pc+1].Op() != OP_EXTRAARG {
				return fmt.Errorf("verify: pc %d NEWTABLE missing EXTRAARG", pc)
			}
			if inst.VB() > 24 {
				return fmt.Errorf("verify: pc %d NEWTABLE vB=%d too large", pc, inst.VB())
			}
			if inst.K() && p.Code[pc+1].Ax() > (1<<18) {
				return fmt.Errorf("verify: pc %d NEWTABLE Ax=%d too large", pc, p.Code[pc+1].Ax())
			}
			isExtraArg[pc+1] = true
			pc++
		case OP_SETLIST:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if inst.VB() > 0 && inst.A()+inst.VB() >= maxStack {
				return fmt.Errorf("verify: pc %d SETLIST A=%d vB=%d >= maxstack %d", pc, inst.A(), inst.VB(), maxStack)
			}
			if inst.K() {
				if pc+1 >= n || p.Code[pc+1].Op() != OP_EXTRAARG {
					return fmt.Errorf("verify: pc %d SETLIST missing EXTRAARG", pc)
				}
				isExtraArg[pc+1] = true
				pc++
			}
		case OP_CONCAT:
			if inst.B() < 1 || inst.A()+inst.B() > maxStack {
				return fmt.Errorf("verify: pc %d CONCAT A=%d B=%d out of maxstack %d", pc, inst.A(), inst.B(), maxStack)
			}
		case OP_ADD, OP_SUB, OP_MUL, OP_MOD, OP_POW, OP_DIV, OP_IDIV,
			OP_BAND, OP_BOR, OP_BXOR, OP_SHL, OP_SHR:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "B", inst.B()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "C", inst.C()); err != nil {
				return err
			}
			if pc+1 >= n || p.Code[pc+1].Op() != OP_MMBIN {
				return fmt.Errorf("verify: pc %d %s not followed by MMBIN", pc, OpNames[op])
			}
		case OP_ADDI, OP_SHRI, OP_SHLI:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "B", inst.B()); err != nil {
				return err
			}
			if pc+1 >= n || p.Code[pc+1].Op() != OP_MMBINI {
				return fmt.Errorf("verify: pc %d %s not followed by MMBINI", pc, OpNames[op])
			}
		case OP_ADDK, OP_SUBK, OP_MULK, OP_MODK, OP_POWK, OP_DIVK, OP_IDIVK,
			OP_BANDK, OP_BORK, OP_BXORK:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "B", inst.B()); err != nil {
				return err
			}
			if inst.C() >= len(p.Consts) {
				return fmt.Errorf("verify: pc %d %s C=%d >= %d", pc, OpNames[op], inst.C(), len(p.Consts))
			}
			ck := p.Consts[inst.C()].Kind
			switch op {
			case OP_BANDK, OP_BORK, OP_BXORK:
				if ck != ConstInt {
					return fmt.Errorf("verify: pc %d %s K[%d] must be integer constant", pc, OpNames[op], inst.C())
				}
			default:
				if ck != ConstInt && ck != ConstFloat {
					return fmt.Errorf("verify: pc %d %s K[%d] must be numeric constant", pc, OpNames[op], inst.C())
				}
			}
			if pc+1 >= n || p.Code[pc+1].Op() != OP_MMBINK {
				return fmt.Errorf("verify: pc %d %s not followed by MMBINK", pc, OpNames[op])
			}
		case OP_MMBIN, OP_MMBINI, OP_MMBINK:
			isMMBin[pc] = true
			if pc == 0 || isExtraArg[pc-1] {
				return fmt.Errorf("verify: pc %d %s has no preceding op", pc, OpNames[op])
			}
			prevOp := p.Code[pc-1].Op()
			switch op {
			case OP_MMBIN:
				switch prevOp {
				case OP_ADD, OP_SUB, OP_MUL, OP_MOD, OP_POW, OP_DIV, OP_IDIV,
					OP_BAND, OP_BOR, OP_BXOR, OP_SHL, OP_SHR:
				default:
					return fmt.Errorf("verify: pc %d MMBIN preceded by invalid op %s", pc, OpNames[prevOp])
				}
			case OP_MMBINI:
				switch prevOp {
				case OP_ADDI, OP_SHRI, OP_SHLI:
				default:
					return fmt.Errorf("verify: pc %d MMBINI preceded by invalid op %s", pc, OpNames[prevOp])
				}
			case OP_MMBINK:
				switch prevOp {
				case OP_ADDK, OP_SUBK, OP_MULK, OP_MODK, OP_POWK, OP_DIVK, OP_IDIVK,
					OP_BANDK, OP_BORK, OP_BXORK:
				default:
					return fmt.Errorf("verify: pc %d MMBINK preceded by invalid op %s", pc, OpNames[prevOp])
				}
			}
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if op == OP_MMBIN {
				if err := checkReg(pc, op, "B", inst.B()); err != nil {
					return err
				}
			} else if op == OP_MMBINK && inst.B() >= len(p.Consts) {
				return fmt.Errorf("verify: pc %d MMBINK B=%d >= %d", pc, inst.B(), len(p.Consts))
			}
			// M3: restrict MMBIN* C strictly to TM_ADD..TM_SHR (6..17).
			if inst.C() < 6 || inst.C() > 17 {
				return fmt.Errorf("verify: pc %d %s invalid binary TM C=%d", pc, OpNames[op], inst.C())
			}
		case OP_JMP:
			target := pc + 1 + inst.SJ()
			if target < 0 || target >= n {
				return fmt.Errorf("verify: pc %d JMP target %d out of [0,%d)", pc, target, n)
			}
		case OP_FORPREP:
			if inst.A()+2 >= maxStack {
				return fmt.Errorf("verify: pc %d FORPREP A=%d out of maxstack %d", pc, inst.A(), maxStack)
			}
			loopPC := pc + 1 + inst.Bx()
			skipPC := loopPC + 1
			if loopPC < 0 || skipPC >= n {
				return fmt.Errorf("verify: pc %d FORPREP target %d out of [0,%d)", pc, skipPC, n)
			}
			if p.Code[loopPC].Op() != OP_FORLOOP || p.Code[loopPC].A() != inst.A() {
				return fmt.Errorf("verify: pc %d FORPREP target %d is not matching FORLOOP", pc, loopPC)
			}
		case OP_FORLOOP:
			if inst.A()+2 >= maxStack {
				return fmt.Errorf("verify: pc %d FORLOOP A=%d out of maxstack %d", pc, inst.A(), maxStack)
			}
			target := pc + 1 - inst.Bx()
			if target < 0 || target >= n {
				return fmt.Errorf("verify: pc %d FORLOOP target %d out of [0,%d)", pc, target, n)
			}
		case OP_TFORPREP:
			if inst.A()+3 >= maxStack {
				return fmt.Errorf("verify: pc %d TFORPREP A=%d out of maxstack %d", pc, inst.A(), maxStack)
			}
			target := pc + 1 + inst.Bx()
			if target < 0 || target >= n {
				return fmt.Errorf("verify: pc %d TFORPREP target %d out of [0,%d)", pc, target, n)
			}
			if p.Code[target].Op() != OP_TFORCALL || p.Code[target].A() != inst.A() {
				return fmt.Errorf("verify: pc %d TFORPREP target %d is not matching TFORCALL", pc, target)
			}
		case OP_TFORCALL:
			// Lua 5.5 TFORCALL copies iterator+args into R[A+3..A+5] before invoking,
			// then writes C results starting at R[A+3].
			if inst.A()+5 >= maxStack || (inst.C() > 0 && inst.A()+2+inst.C() >= maxStack) {
				return fmt.Errorf("verify: pc %d TFORCALL A=%d C=%d out of maxstack %d", pc, inst.A(), inst.C(), maxStack)
			}
			if pc+1 >= n || p.Code[pc+1].Op() != OP_TFORLOOP {
				return fmt.Errorf("verify: pc %d TFORCALL not followed by TFORLOOP", pc)
			}
		case OP_TFORLOOP:
			if inst.A()+3 >= maxStack {
				return fmt.Errorf("verify: pc %d TFORLOOP A=%d out of maxstack %d", pc, inst.A(), maxStack)
			}
			target := pc + 1 - inst.Bx()
			if target < 0 || target >= n {
				return fmt.Errorf("verify: pc %d TFORLOOP target %d out of [0,%d)", pc, target, n)
			}
		case OP_CLOSURE:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			bx := inst.Bx()
			if bx >= len(p.Protos) {
				return fmt.Errorf("verify: pc %d CLOSURE Bx=%d >= %d", pc, bx, len(p.Protos))
			}
			if !verifiedClosure[bx] {
				sub := p.Protos[bx]
				if sub == nil {
					return fmt.Errorf("verify: pc %d CLOSURE nil sub-proto %d", pc, bx)
				}
				if len(sub.Upvalues) > MaxArgA {
					return fmt.Errorf("verify: pc %d CLOSURE sub-proto %d too many upvalues %d > %d", pc, bx, len(sub.Upvalues), MaxArgA)
				}
				for ui, ud := range sub.Upvalues {
					if ud.InStack == 0 {
						if int(ud.Idx) >= len(p.Upvalues) {
							return fmt.Errorf("verify: pc %d CLOSURE sub upval[%d] idx=%d >= %d", pc, ui, ud.Idx, len(p.Upvalues))
						}
					} else if int(ud.Idx) >= maxStack {
						return fmt.Errorf("verify: pc %d CLOSURE sub upval[%d] reg=%d >= maxstack %d", pc, ui, ud.Idx, maxStack)
					}
				}
				verifiedClosure[bx] = true
			}
		case OP_CALL, OP_TAILCALL:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if inst.B() > 0 && inst.A()+inst.B() > maxStack {
				return fmt.Errorf("verify: pc %d %s A=%d B=%d > maxstack %d", pc, OpNames[op], inst.A(), inst.B(), maxStack)
			}
			if op == OP_CALL && inst.C() >= 2 && inst.A()+inst.C()-2 >= maxStack {
				return fmt.Errorf("verify: pc %d CALL results A=%d C=%d >= maxstack %d", pc, inst.A(), inst.C(), maxStack)
			}
			p.ICSlot[pc] = int32(icCount)
			icCount++
		case OP_RETURN:
			if inst.B() == 0 {
				if err := checkReg(pc, op, "A", inst.A()); err != nil {
					return err
				}
			} else if inst.A()+inst.B()-1 > maxStack {
				return fmt.Errorf("verify: pc %d RETURN A=%d B=%d > maxstack %d", pc, inst.A(), inst.B(), maxStack)
			}
		case OP_VARARGPREP:
			if pc != 0 {
				return fmt.Errorf("verify: pc %d VARARGPREP only valid at pc 0", pc)
			}
			if p.Flag&PFVarargTable != 0 && int(p.NumParams) >= maxStack {
				return fmt.Errorf("verify: pc %d VARARGPREP vtab slot %d >= maxstack %d", pc, p.NumParams, maxStack)
			}
		case OP_VARARG:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if inst.K() {
				if err := checkReg(pc, op, "B", inst.B()); err != nil {
					return err
				}
			} else if p.Flag&PFVarargHidden == 0 {
				return fmt.Errorf("verify: pc %d VARARG k=0 in proto without PFVarargHidden", pc)
			}
			if inst.C() >= 2 && inst.A()+inst.C()-2 >= maxStack {
				return fmt.Errorf("verify: pc %d VARARG A=%d C=%d >= maxstack %d", pc, inst.A(), inst.C(), maxStack)
			}
		case OP_GETVARG:
			if p.Flag&PFVarargHidden == 0 {
				return fmt.Errorf("verify: pc %d GETVARG in proto without PFVarargHidden", pc)
			}
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if err := checkReg(pc, op, "C", inst.C()); err != nil {
				return err
			}
		case OP_ERRNNIL:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			if bx := inst.Bx(); bx > 0 && bx-1 >= len(p.Consts) {
				return fmt.Errorf("verify: pc %d ERRNNIL Bx=%d out of bounds", pc, bx)
			}
		case OP_EQ, OP_LT, OP_LE, OP_EQK, OP_EQI, OP_LTI, OP_LEI, OP_GTI, OP_GEI, OP_TEST, OP_TESTSET:
			if err := checkReg(pc, op, "A", inst.A()); err != nil {
				return err
			}
			switch op {
			case OP_EQ, OP_LT, OP_LE, OP_TESTSET:
				if err := checkReg(pc, op, "B", inst.B()); err != nil {
					return err
				}
			case OP_EQK:
				if inst.B() >= len(p.Consts) {
					return fmt.Errorf("verify: pc %d EQK B=%d >= %d", pc, inst.B(), len(p.Consts))
				}
			}
			// M1: conditional op requires OP_JMP at pc+1 AND a valid fallthrough instruction at pc+2!
			if pc+1 >= n || p.Code[pc+1].Op() != OP_JMP {
				return fmt.Errorf("verify: pc %d %s not followed by JMP", pc, OpNames[op])
			}
			if pc+2 >= n {
				return fmt.Errorf("verify: pc %d %s fallthrough skips past end of code (%d)", pc, OpNames[op], n)
			}
		}
	}

	// Second pass (M1): verify NO control-flow target or skip-1 fallthrough lands on EXTRAARG or unpaired MMBIN*.
	checkTarget := func(pc int, op OpCode, target int) error {
		if target < 0 || target >= n {
			return fmt.Errorf("verify: pc %d %s target %d out of [0,%d)", pc, OpNames[op], target, n)
		}
		if isExtraArg[target] {
			return fmt.Errorf("verify: pc %d %s lands on EXTRAARG at %d", pc, OpNames[op], target)
		}
		if isMMBin[target] {
			return fmt.Errorf("verify: pc %d %s lands on MMBIN* at %d", pc, OpNames[op], target)
		}
		return nil
	}
	for pc := 0; pc < n; pc++ {
		if cancelFn != nil && pc > 0 && (pc&4095) == 0 {
			if err := verifyTick(cancelFn, work, 4096); err != nil {
				return err
			}
		}
		if isExtraArg[pc] {
			continue
		}
		inst := p.Code[pc]
		op := inst.Op()
		switch op {
		case OP_JMP:
			if err := checkTarget(pc, op, pc+1+inst.SJ()); err != nil {
				return err
			}
		case OP_FORPREP:
			if err := checkTarget(pc, op, pc+1+inst.Bx()+1); err != nil {
				return err
			}
		case OP_FORLOOP:
			if err := checkTarget(pc, op, pc+1-inst.Bx()); err != nil {
				return err
			}
		case OP_TFORPREP:
			if err := checkTarget(pc, op, pc+1+inst.Bx()); err != nil {
				return err
			}
		case OP_TFORLOOP:
			if err := checkTarget(pc, op, pc+1-inst.Bx()); err != nil {
				return err
			}
		case OP_LFALSESKIP:
			if err := checkTarget(pc, op, pc+2); err != nil {
				return err
			}
		case OP_EQ, OP_LT, OP_LE, OP_EQK, OP_EQI, OP_LTI, OP_LEI, OP_GTI, OP_GEI, OP_TEST, OP_TESTSET:
			if err := checkTarget(pc, op, pc+2); err != nil {
				return err
			}
		}
	}

	p.ICCount = icCount

	// Last instruction must terminate control flow so hot dispatch never reads past len(Code).
	last := p.Code[n-1].Op()
	switch last {
	case OP_RETURN, OP_RETURN0, OP_RETURN1, OP_TAILCALL, OP_JMP:
	default:
		return fmt.Errorf("verify: last instruction %s does not terminate", OpNames[last])
	}

	for _, sub := range p.Protos {
		if err := verifyDepth(sub, depth+1, checkMem, memUsed, cancelFn, work); err != nil {
			return err
		}
	}
	return nil
}
