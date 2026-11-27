package ir

import "github.com/yuchanns/moelua/bytecode"

// LoopCandidate describes an innermost natural loop found in bytecode.Proto.Code.
type LoopCandidate struct {
	Kind         LoopKind
	EntryPC      int   // OP_FORPREP PC (LoopForInt) or header PC (LoopWhile)
	BodyPC       int   // First instruction of loop body
	EndPC        int   // OP_FORLOOP PC or backward OP_JMP PC
	ExitPC       int   // EndPC + 1
	ForBase      uint8 // A register of OP_FORPREP/OP_FORLOOP
	HasConstStep bool
	ConstStep    int64
}

// DiscoverLoops scans p.Code for innermost natural loops (both counted numeric `for`
// loops and single-backedge `while`/`repeat` loops) that contain no nested loops.
func DiscoverLoops(p *bytecode.Proto) []LoopCandidate {
	code := p.Code
	n := len(code)
	if n < 2 {
		return nil
	}

	var raw []LoopCandidate
	for pc := 0; pc < n; pc++ {
		w := code[pc]
		switch w.Op() {
		case bytecode.OP_FORLOOP:
			bodyPC := pc + 1 - w.Bx()
			entryPC := bodyPC - 1
			if entryPC < 0 || entryPC >= pc {
				continue
			}
			prep := code[entryPC]
			if prep.Op() != bytecode.OP_FORPREP || prep.A() != w.A() {
				continue
			}
			if entryPC+1+prep.Bx()+1 != pc+1 {
				// Non-matching FORPREP/FORLOOP pair
				continue
			}
			cand := LoopCandidate{
				Kind:    LoopForInt,
				EntryPC: entryPC,
				BodyPC:  bodyPC,
				EndPC:   pc,
				ExitPC:  pc + 1,
				ForBase: uint8(w.A()),
			}
			// Detect constant step loaded immediately before FORPREP (standard Lua `for i=a,b do`).
			if entryPC > 0 {
				prev := code[entryPC-1]
				if prev.Op() == bytecode.OP_LOADI && uint8(prev.A()) == cand.ForBase+2 {
					cand.HasConstStep = true
					cand.ConstStep = int64(prev.SBx())
				} else if prev.Op() == bytecode.OP_LOADK && uint8(prev.A()) == cand.ForBase+2 {
					bx := prev.Bx()
					if bx < len(p.Consts) && p.Consts[bx].Kind == bytecode.ConstInt {
						cand.HasConstStep = true
						cand.ConstStep = p.Consts[bx].Int
					}
				}
			}
			raw = append(raw, cand)

		case bytecode.OP_JMP:
			sj := w.SJ()
			if sj >= 0 {
				continue
			}
			// Ignore backward JMP immediately preceded by a fused comparison (would be an unusual repeat/while;
			// standard Lua emits a standalone unconditional backedge OP_JMP at the bottom of while/repeat loops).
			if pc > 0 && isCompareOrTestOp(code[pc-1].Op()) {
				continue
			}
			target := pc + 1 + sj
			if target < 0 || target >= pc {
				continue
			}
			// Require the loop header not to land in the middle of an MMBIN or fused JMP.
			if isMMBinOp(code[target].Op()) || (target > 0 && isCompareOrTestOp(code[target-1].Op())) {
				continue
			}
			raw = append(raw, LoopCandidate{
				Kind:    LoopWhile,
				EntryPC: target,
				BodyPC:  target,
				EndPC:   pc,
				ExitPC:  pc + 1,
			})
		}
	}

	if len(raw) == 0 {
		return nil
	}

	// Filter to innermost loops with single entry (no external jump into the middle of the body,
	// no nested backedges inside [BodyPC, EndPC-1], and bounded size).
	var out []LoopCandidate
	for i, c := range raw {
		if c.EndPC-c.BodyPC > 160 {
			continue
		}
		if hasNestedLoop(raw, i) {
			continue
		}
		if hasInternalBackedgeOrExternalEntry(code, c) {
			continue
		}
		// Avoid overlapping ranges with already-accepted candidates.
		overlap := false
		for _, prev := range out {
			if c.EntryPC <= prev.EndPC && prev.EntryPC <= c.EndPC {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		out = append(out, c)
	}
	return out
}

func hasNestedLoop(all []LoopCandidate, idx int) bool {
	c := all[idx]
	for j, o := range all {
		if j == idx {
			continue
		}
		if o.EntryPC >= c.BodyPC && o.EndPC <= c.EndPC {
			return true
		}
	}
	return false
}

func hasInternalBackedgeOrExternalEntry(code []bytecode.Instruction, c LoopCandidate) bool {
	for pc := 0; pc < len(code); pc++ {
		w := code[pc]
		op := w.Op()
		if op == bytecode.OP_JMP {
			target := pc + 1 + w.SJ()
			insideSrc := pc >= c.BodyPC && pc < c.EndPC
			if insideSrc {
				// Reject internal backward jumps (irreducible or nested loops).
				if target <= pc {
					return true
				}
				// Forward jumps must either stay inside [BodyPC, EndPC] or exit cleanly past EndPC.
				if target < c.BodyPC {
					return true
				}
			} else if pc != c.EndPC {
				// External jump must not enter the middle of the loop body.
				if target > c.EntryPC && target <= c.EndPC {
					// Exception: the loop's own FORPREP skips to ExitPC (EndPC+1), which is > EndPC.
					return true
				}
			}
		} else if op == bytecode.OP_FORLOOP || op == bytecode.OP_TFORLOOP {
			if pc >= c.BodyPC && pc < c.EndPC {
				return true
			}
		}
	}
	return false
}

func isCompareOrTestOp(op bytecode.OpCode) bool {
	switch op {
	case bytecode.OP_EQ, bytecode.OP_LT, bytecode.OP_LE,
		bytecode.OP_EQK, bytecode.OP_EQI, bytecode.OP_LTI, bytecode.OP_LEI,
		bytecode.OP_GTI, bytecode.OP_GEI, bytecode.OP_TEST, bytecode.OP_TESTSET:
		return true
	}
	return false
}

func isMMBinOp(op bytecode.OpCode) bool {
	return op == bytecode.OP_MMBIN || op == bytecode.OP_MMBINI || op == bytecode.OP_MMBINK
}
