//go:build (arm64 || amd64) && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import "github.com/yuchanns/moelua/bytecode"

// stepAsmCold executes one instruction that exited either the Plan 9 direct-threaded
// interpreter or the native JIT loop (cold opcode, IC miss, allocation, GC barrier,
// metamethod, vararg call, or native call).
//
//go:noinline
func (s *State) stepAsmCold(th *Thread, ev *frameEnv, ipc int, w bytecode.Instruction, regs *[256]Value, entryDepth int) (nextPC int, reload bool, done bool, err error) {
	pc := ipc + 1
	a := uint8(w >> bytecode.PosA)

	switch bytecode.OpCode(w & 0x7f) {
	case bytecode.OP_GETUPVAL:
		uv := s.closureUpval(ev.cl, int(uint8(w>>bytecode.PosB)))
		if uv.isClosed() {
			regs[a] = uv.closed
		} else if uv.isOwnedBy(th) {
			regs[a] = ev.stack[uv.stackIdx]
		} else {
			regs[a] = s.getCrossThreadUpval(uv)
		}
		return pc, false, false, nil

	case bytecode.OP_SETUPVAL:
		uv := s.closureUpval(ev.cl, int(uint8(w>>bytecode.PosB)))
		val := regs[a]
		if uv.isClosed() {
			s.setClosedUpvalVal(uv, val)
		} else if uv.isOwnedBy(th) {
			ev.stack[uv.stackIdx] = val
		} else {
			s.setCrossThreadUpval(uv, val)
		}
		return pc, false, false, nil

	case bytecode.OP_GETTABUP:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.getTabUpSlow(th, ev, ipc, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_GETFIELD:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.getFieldSlow(th, ev, ipc, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_SETFIELD:
		ra := regs[a]
		if ra.IsTable() {
			t := s.asTable(ra)
			var val Value
			if w.K() {
				val = ev.consts[uint8(w>>bytecode.PosC)]
			} else {
				val = regs[uint8(w>>bytecode.PosC)]
			}
			ic := icAt(ev.bp.ic, ev.bp.icSlots[ipc])
			if ic.setFieldFast(s, t, val, s.protoEpoch) {
				return pc, false, false, nil
			}
		}
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.setFieldSlow(th, ev, ipc, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_NEWTABLE:
		if !s.gcNeedsStep() && s.memLimit == 0 && !w.K() && w.VC() == 0 && w.VB() <= 2 {
			if t := s.newSmallRecordTable(); t != nil {
				regs[a] = TableValue(t)
				return pc + 1, false, false, nil
			}
		}
		goto cold

	case bytecode.OP_SELF:
		regs[a+1] = regs[uint8(w>>bytecode.PosB)]
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.selfSlow(th, ev, ipc, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_GETI:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.getISlow(th, ev, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_GETTABLE:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.getTableSlow(th, ev, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_SETI:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.setISlow(th, ev, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_SETTABLE:
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.setTableSlow(th, ev, w, regs)
		return pc, pushed, false, serr

	case bytecode.OP_ADDI:
		// Reached only when rb is neither int nor float: fall through to OP_MMBINI at pc.
		return pc, false, false, nil

	case bytecode.OP_LEN:
		rb := regs[uint8(w>>bytecode.PosB)]
		if rb.IsTable() {
			t := s.asTable(rb)
			if !t.hasMeta() {
				regs[a].SetInt(t.Len(s))
				return pc, false, false, nil
			}
		} else if rb.IsString() {
			regs[a].SetInt(int64(s.asString(rb).Len))
			return pc, false, false, nil
		}
		goto cold

	case bytecode.OP_CALL:
		fn := regs[a]
		b := int(uint8(w >> bytecode.PosB))
		c := int(uint8(w >> bytecode.PosC))
		nargs := b - 1
		if b == 0 {
			nargs = max(0, th.top-(ev.base+int(a)+1))
		}
		if fn.IsNative() {
			nf := s.asNative(fn)
			if nf.Intrinsic != intrNone && (c == 2 || c == 0) && nargs >= 1 {
				if execIntrinsic1(s, nf.Intrinsic, regs, a, nargs) {
					if c == 0 {
						th.top = ev.base + int(a) + 1
					} else if b == 0 {
						if end := ev.base + int(ev.bp.maxStack); th.top < end {
							th.top = end
						}
					}
					return pc, false, false, nil
				}
			}
		}
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.callSlow(th, ev, w, int(a), nargs, c-1)
		return pc, pushed, false, serr

	case bytecode.OP_FORPREP:
		ev.frames[ev.ci].pc = pc
		skip, serr := s.forPrepSlow(regs, a)
		if serr != nil {
			if s.handleError(th, entryDepth, serr) {
				return 0, true, false, nil
			}
			return 0, false, false, serr
		}
		if skip {
			pc += w.Bx() + 1
		}
		return pc, false, false, nil

	case bytecode.OP_TFORCALL:
		fn := regs[a]
		c := int(uint8(w >> bytecode.PosC))
		if fn.IsNative() && s.asNative(fn).Intrinsic == intrIPairsIter && regs[a+1].IsTable() {
			t := s.asTable(regs[a+1])
			if !t.hasMeta() && regs[a+3].IsInt() {
				ni := regs[a+3].Int() + 1
				v := t.RawGetInt(s, ni)
				if v.IsNil() {
					regs[a+3].SetNil()
				} else {
					regs[a+3].SetInt(ni)
					if c >= 2 {
						regs[a+4] = v
						for k := 2; k < c; k++ {
							regs[int(a)+3+k].SetNil()
						}
					}
				}
				w2 := ev.insns.at(pc)
				pc++
				if !regs[a+3].IsNil() {
					pc -= w2.Bx()
				}
				return pc, false, false, nil
			}
		}
		ev.frames[ev.ci].pc = pc
		pushed, serr := s.tforCallSlow(th, ev, a, c)
		return pc, pushed, false, serr
	}

cold:
	ev.frames[ev.ci].pc = pc
	action, newPC, cerr := s.coldOp(th, ev, ipc, w, regs, entryDepth)
	if cerr != nil {
		return 0, false, false, cerr
	}
	switch action {
	case coldContinue:
		return newPC, false, false, nil
	case coldReload:
		return 0, true, false, nil
	case coldReturn:
		return 0, false, true, nil
	}
	return pc, false, false, nil
}
