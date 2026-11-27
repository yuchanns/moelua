//go:build (arm64 || amd64) && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/arena"
	"github.com/yuchanns/moelua/internal/jit/ir"
)

// protoNeedsFeedback reports whether any innermost loop in p contains dynamic operations
// (field access, calls, or table indexing) that benefit from a one-time Tier-2 recompilation
// with warmed Shape ICs, observed array kinds, and inlinable leaf callee prototypes.
func protoNeedsFeedback(p *bytecode.Proto) bool {
	cands := ir.DiscoverLoops(p)
	for _, c := range cands {
		for pc := c.BodyPC; pc < c.EndPC; pc++ {
			switch p.Code[pc].Op() {
			case bytecode.OP_GETFIELD, bytecode.OP_SETFIELD, bytecode.OP_CALL,
				bytecode.OP_GETTABLE, bytecode.OP_SETTABLE, bytecode.OP_GETI, bytecode.OP_SETI:
				return true
			}
		}
	}
	return false
}

// collectJITFeedback harvests live monomorphic Shape ICs, table array kinds, and pure leaf
// Lua callees from the current frame and boundProto for Tier-2 optimizing loop compilation.
func collectJITFeedback(s *State, cl *Closure, bp *boundProto, regs *[256]Value, enableSIMD bool) *ir.Feedback {
	fb := &ir.Feedback{
		EnableSIMD: enableSIMD,
	}
	p := bp.proto
	if p == nil {
		return fb
	}

	// 1. Harvest warmed monomorphic own-slot Shape ICs for GETFIELD/SETFIELD.
	for pc, slot := range bp.icSlots {
		if slot < 0 || int(slot) >= len(bp.ic) || pc >= len(p.Code) {
			continue
		}
		op := p.Code[pc].Op()
		if op != bytecode.OP_GETFIELD && op != bytecode.OP_SETFIELD {
			continue
		}
		ic := &bp.ic[slot]
		if ic.kind == icOwnSlot && ic.shapeRef != arena.NullRef && ic.slot >= 0 {
			if fb.FieldICs == nil {
				fb.FieldICs = make(map[int]ir.FieldFeedback, 8)
			}
			fb.FieldICs[pc] = ir.FieldFeedback{
				ShapeRef: uint32(ic.shapeRef),
				Slot:     uint16(ic.slot),
			}
		}
	}

	// 2. Harvest live Table.arrKind from frame registers.
	if regs != nil {
		limit := int(bp.maxStack)
		if limit > 256 {
			limit = 256
		}
		for r := 0; r < limit; r++ {
			v := regs[r]
			if v.IsTable() {
				t := s.asTable(v)
				if t != nil && t.arrKind != arrEmpty {
					if fb.ArrKinds == nil {
						fb.ArrKinds = make(map[uint8]uint8, 8)
					}
					fb.ArrKinds[uint8(r)] = uint8(t.arrKind)
				}
			}
		}
	}

	// 3. Harvest live upvalues (Table.arrKind and pure leaf Lua callees for OP_CALL inlining).
	var upvalVals []Value
	if cl != nil && cl.upvalsRef != arena.NullRef && cl.nUpvals > 0 {
		upvalVals = make([]Value, cl.nUpvals)
		for i := range int(cl.nUpvals) {
			uv := s.closureUpval(cl, i)
			if uv == nil {
				continue
			}
			v := uv.Get(s)
			upvalVals[i] = v
			if v.IsTable() {
				t := s.asTable(v)
				if t != nil && t.arrKind != arrEmpty {
					if fb.UpvalArrs == nil {
						fb.UpvalArrs = make(map[int]uint8, 4)
					}
					fb.UpvalArrs[i] = uint8(t.arrKind)
				}
			}
		}
	}

	// 4. Identify small pure leaf Lua callees at OP_CALL inside loops.
	cands := ir.DiscoverLoops(p)
	for _, cand := range cands {
		for pc := cand.BodyPC; pc < cand.EndPC; pc++ {
			w := p.Code[pc]
			if w.Op() != bytecode.OP_CALL || w.C() != 2 || w.B() < 1 {
				continue
			}
			a := uint8(w.A())
			var callee Value
			// Check if immediately preceded by GETUPVAL writing slot `a`.
			foundUV := false
			for back := pc - 1; back >= cand.BodyPC && back >= pc-4; back-- {
				bw := p.Code[back]
				if bw.Op() == bytecode.OP_GETUPVAL && uint8(bw.A()) == a {
					uv := bw.B()
					if uv < len(upvalVals) {
						callee = upvalVals[uv]
						foundUV = true
					}
					break
				}
			}
			if !foundUV && regs != nil {
				callee = regs[a]
			}
			if !callee.IsClosure() {
				continue
			}
			ccl := s.asClosure(callee)
			if ccl == nil {
				continue
			}
			cbp := s.boundProtoOf(ccl)
			if cbp == nil || cbp.proto == nil || cbp.isVararg {
				continue
			}
			if fb.InlineCalls == nil {
				fb.InlineCalls = make(map[int]*ir.InlineTarget, 4)
			}
			fb.InlineCalls[pc] = &ir.InlineTarget{
				ProtoIdx:  ccl.protoIdx,
				Proto:     cbp.proto,
				NumParams: int(cbp.numParams),
			}
		}
	}

	return fb
}
