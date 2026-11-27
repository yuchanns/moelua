package ir

// OptimizeRegion runs loop-level SSA optimizations on reg in place:
//  1. Preheader constant deduplication (CSE on preheader constants/guards)
//  2. Per-block pure expression & alias-safe array/slot load CSE
//  3. Bounds-check elimination (BCE) via hoisted preheader OpGuardRange for counted for loops
//  4. Dead-code elimination (DCE) across body blocks and preheader
//  5. Deopt-safety verification & non-aliasing LoadSlot hoisting (guarantees every mid-loop
//     deopt occurs before any state mutation in the current iteration so baseline replay at
//     BodyPC is 100% exact).
//
// Returns false if a mid-loop deoptable op cannot be proven to precede all mutations.
func OptimizeRegion(reg *LoopRegion) bool {
	if reg == nil {
		return false
	}
	dedupPreheader(reg)
	runBlockCSE(reg)
	markWrittenShapes(reg)
	runBCE(reg)
	runDCE(reg)
	return verifyOrHoistDeoptSafety(reg)
}

// markWrittenShapes tags any OpGuardShape whose result is written by OpStoreSlot*
// with ImmI = 1 so native codegen guards t.metaRef == 0 before any mutation (preserving
// Lua 5.5 __newindex semantics if an existing own slot currently holds nil).
func markWrittenShapes(reg *LoopRegion) {
	written := make(map[ValID]bool, 4)
	for _, blk := range reg.Blocks {
		for i := range blk.Insts {
			in := &blk.Insts[i]
			if !in.Dead && (in.Op == OpStoreSlotFloat || in.Op == OpStoreSlotInt) {
				written[in.Args[0]] = true
			}
		}
	}
	if len(written) == 0 {
		return
	}
	for _, blk := range reg.Blocks {
		for i := range blk.Insts {
			in := &blk.Insts[i]
			if !in.Dead && in.Op == OpGuardShape && written[in.ID] {
				in.ImmI = 1
			}
		}
	}
}

func canDeoptMidLoop(in *Inst) bool {
	switch in.Op {
	case OpIDivI, OpModI, OpLoadArrTable, OpGuardShape, OpLoadSlotFloat, OpLoadSlotInt:
		return true
	case OpLoadArrInt, OpLoadArrFloat, OpLoadArrBool, OpStoreArrInt, OpStoreArrFlt, OpStoreArrBool:
		return !in.BCE
	}
	return false
}

func mutatesState(op Op) bool {
	switch op {
	case OpSetVar, OpStoreArrInt, OpStoreArrFlt, OpStoreArrBool, OpStoreSlotFloat, OpStoreSlotInt:
		return true
	}
	return false
}

// verifyOrHoistDeoptSafety ensures that no mid-loop deoptable instruction can execute after
// any mutable state (LoopVar or Table memory) has been modified in the current iteration.
// For single-block object-update loops (e.g. NBody updating bj.vx/vy/vz after bivx/bivy/bivz),
// it hoists non-aliasing OpLoadSlot* reads on an already-guarded shape object above the first
// mutation so all field type guards run atomically before any write commits.
func verifyOrHoistDeoptSafety(reg *LoopRegion) bool {
	// Single-block hoist: lift OpLoadSlot* whose shape obj was defined before the first mutation
	// and whose slot index was not previously stored in this block.
	if len(reg.Blocks) == 1 {
		blk := reg.Blocks[0]
		firstMut := -1
		for i := range blk.Insts {
			if !blk.Insts[i].Dead && mutatesState(blk.Insts[i].Op) {
				firstMut = i
				break
			}
		}
		if firstMut >= 0 {
			defIdx := make(map[ValID]int, len(blk.Insts))
			for i := range blk.Insts {
				defIdx[blk.Insts[i].ID] = i
			}
			type slotKey struct {
				obj  ValID
				slot int32
			}
			storedSlots := make(map[slotKey]bool)
			var hoisted []Inst
			hoistedAt := make(map[int]bool)
			for i := firstMut; i < len(blk.Insts); i++ {
				in := &blk.Insts[i]
				if in.Dead {
					continue
				}
				if in.Op == OpStoreSlotFloat || in.Op == OpStoreSlotInt {
					storedSlots[slotKey{in.Args[0], in.AuxIdx}] = true
					continue
				}
				if in.Op == OpLoadSlotFloat || in.Op == OpLoadSlotInt {
					obj := in.Args[0]
					if di, ok := defIdx[obj]; ok && di < firstMut && !storedSlots[slotKey{obj, in.AuxIdx}] {
						hoisted = append(hoisted, *in)
						hoistedAt[i] = true
					}
				}
			}
			if len(hoisted) > 0 {
				reordered := make([]Inst, 0, len(blk.Insts))
				reordered = append(reordered, blk.Insts[:firstMut]...)
				reordered = append(reordered, hoisted...)
				for i := firstMut; i < len(blk.Insts); i++ {
					if !hoistedAt[i] {
						reordered = append(reordered, blk.Insts[i])
					}
				}
				blk.Insts = reordered
			}
		}
	}

	// Verify globally: in block 0, no mid-loop deopt after first mutation; in blocks > 0,
	// no mid-loop deopt if any prior block (or earlier in same block) mutated state.
	mutatedBefore := false
	for bi, blk := range reg.Blocks {
		seenMut := mutatedBefore
		for i := range blk.Insts {
			in := &blk.Insts[i]
			if in.Dead {
				continue
			}
			if canDeoptMidLoop(in) && seenMut {
				return false
			}
			if mutatesState(in.Op) {
				seenMut = true
				if bi == 0 {
					mutatedBefore = true
				}
			}
		}
		if seenMut {
			mutatedBefore = true
		}
	}
	return true
}

type preKey struct {
	op       Op
	typ      Type
	slot     uint8
	upvalIdx int16
	auxIdx   int32
	auxU32   uint32
	immI     int64
	immU     uint64
	arg0     ValID
}

func dedupPreheader(reg *LoopRegion) {
	seen := make(map[preKey]ValID, len(reg.Preheader))
	remap := make(map[ValID]ValID, 8)

	for i := range reg.Preheader {
		in := &reg.Preheader[i]
		rewriteArgs(in, remap)
		if !isDedupablePreOp(in.Op) {
			continue
		}
		k := preKey{
			op:       in.Op,
			typ:      in.Type,
			slot:     in.Slot,
			upvalIdx: in.UpvalIdx,
			auxIdx:   in.AuxIdx,
			auxU32:   in.AuxU32,
			immI:     in.ImmI,
			immU:     in.ImmU,
			arg0:     in.Args[0],
		}
		if prev, ok := seen[k]; ok {
			remap[in.ID] = prev
			in.Dead = true
		} else {
			seen[k] = in.ID
		}
	}
	if len(remap) == 0 {
		return
	}
	applyRemap(reg, remap)
}

func isDedupablePreOp(op Op) bool {
	switch op {
	case OpConstInt, OpConstFloat, OpConstBool,
		OpUnboxInt, OpUnboxFloat, OpUnboxNumFloat, OpUnboxBool,
		OpGuardTableArr, OpLoadArrLen,
		OpGuardUpvalInt, OpGuardUpvalFlt, OpGuardUpvalSqrt, OpGuardUpvalAbs,
		OpGuardRegSqrt, OpGuardRegAbs:
		return true
	}
	return false
}

type cseKey struct {
	op      Op
	typ     Type
	a0      ValID
	a1      ValID
	immI    int64
	immU    uint64
	auxIdx  int32
	auxU32  uint32
	auxCond CmpCond
}

// runBlockCSE eliminates duplicate pure expressions and alias-safe array/slot loads within each basic block.
// Notably eliminates redundant loads in `C[rowA+j] = C[rowA+j] + ...`, `vi[k] = vi[k] - ...`, and `bj.vx = bj.vx + ...`.
func runBlockCSE(reg *LoopRegion) {
	remap := make(map[ValID]ValID, 16)
	for _, blk := range reg.Blocks {
		pureSeen := make(map[cseKey]ValID, len(blk.Insts))
		memSeen := make(map[cseKey]ValID, 8)
		for i := range blk.Insts {
			in := &blk.Insts[i]
			if in.Dead {
				continue
			}
			rewriteArgs(in, remap)
			switch in.Op {
			case OpStoreArrInt, OpStoreArrFlt, OpStoreArrBool, OpStoreSlotFloat, OpStoreSlotInt:
				// Any memory store conservatively invalidates cached loads in this block
				// because two Lua registers may alias the same backing Table at runtime.
				clear(memSeen)
			case OpLoadArrInt, OpLoadArrFloat, OpLoadArrBool, OpLoadArrTable,
				OpLoadSlotFloat, OpLoadSlotInt:
				k := cseKey{op: in.Op, typ: in.Type, a0: in.Args[0], a1: in.Args[1], auxIdx: in.AuxIdx}
				if prev, ok := memSeen[k]; ok {
					remap[in.ID] = prev
					in.Dead = true
				} else {
					memSeen[k] = in.ID
				}
			case OpConstInt, OpConstFloat, OpConstBool,
				OpAddI, OpSubI, OpMulI, OpNegI, OpBNotI, OpAndI, OpOrI, OpXorI, OpShlI, OpShrI, OpAddIImm, OpIntToFloat,
				OpAddF, OpSubF, OpMulF, OpDivF, OpNegF, OpSqrtF, OpAbsF, OpAbsI,
				OpCmpI, OpCmpF, OpNotB, OpGuardShape:
				a0, a1 := in.Args[0], in.Args[1]
				if isCommutative(in.Op) && a1 < a0 {
					a0, a1 = a1, a0
				}
				k := cseKey{
					op:      in.Op,
					typ:     in.Type,
					a0:      a0,
					a1:      a1,
					immI:    in.ImmI,
					immU:    in.ImmU,
					auxIdx:  in.AuxIdx,
					auxU32:  in.AuxU32,
					auxCond: in.AuxCond,
				}
				if prev, ok := pureSeen[k]; ok {
					remap[in.ID] = prev
					in.Dead = true
				} else {
					pureSeen[k] = in.ID
				}
			}
		}
		if blk.Term.CondVal != NoVal {
			if r, ok := remap[blk.Term.CondVal]; ok {
				blk.Term.CondVal = r
			}
		}
	}
	if len(remap) > 0 {
		applyRemap(reg, remap)
	}
}

func isCommutative(op Op) bool {
	switch op {
	case OpAddI, OpMulI, OpAndI, OpOrI, OpXorI, OpAddF, OpMulF:
		return true
	}
	return false
}

// runBCE performs Bounds-Check Elimination for counted integer for loops (LoopForInt).
// For any array access whose index is affine `BaseOff + LoopIdx + Delta`, we aggregate
// `[minDelta, maxDelta]` per `(arrVal, BaseOff)` across the loop, emit ONE preheader
// OpGuardRange check, and mark every covered load/store with BCE = true!
func runBCE(reg *LoopRegion) {
	if reg.Kind != LoopForInt {
		return
	}
	type rangeKey struct {
		arrVal  ValID
		baseOff ValID
	}
	type rangeSpan struct {
		minDelta int64
		maxDelta int64
		instPtrs []*Inst
	}
	spans := make(map[rangeKey]*rangeSpan, 8)
	var order []rangeKey

	for _, blk := range reg.Blocks {
		for i := range blk.Insts {
			in := &blk.Insts[i]
			if in.Dead {
				continue
			}
			switch in.Op {
			case OpLoadArrInt, OpLoadArrFloat, OpLoadArrBool, OpLoadArrTable,
				OpStoreArrInt, OpStoreArrFlt, OpStoreArrBool:
				arrV := in.Args[0]
				idxV := in.Args[1]
				af, ok := reg.Affine[idxV]
				if !ok || !af.Valid {
					continue
				}
				// Only hoist range guards for accesses guaranteed to execute (block 0 / single-block)
				// OR whose array is guaranteed valid (in Lua, if a conditional branch inside a loop
				// never executes, an OOB access inside the untaken branch must NOT trigger a guard
				// failure, EXCEPT guard failure simply falls back to baseline which executes identically!).
				// Wait: preheader guard failure ALWAYS falls back cleanly to baseline with 0 side effects!
				// Still, only hoist BCE when delta is reasonable (`[-16, +16]`).
				if af.Delta < -16 || af.Delta > 16 {
					continue
				}
				rk := rangeKey{arrVal: arrV, baseOff: af.BaseOff}
				sp, exists := spans[rk]
				if !exists {
					sp = &rangeSpan{minDelta: af.Delta, maxDelta: af.Delta}
					spans[rk] = sp
					order = append(order, rk)
				} else {
					if af.Delta < sp.minDelta {
						sp.minDelta = af.Delta
					}
					if af.Delta > sp.maxDelta {
						sp.maxDelta = af.Delta
					}
				}
				sp.instPtrs = append(sp.instPtrs, in)
			}
		}
	}

	for _, rk := range order {
		sp := spans[rk]
		id := reg.newVal()
		numArgs := uint8(1)
		if rk.baseOff != NoVal {
			numArgs = 2
		}
		reg.Preheader = append(reg.Preheader, Inst{
			ID:       id,
			Op:       OpGuardRange,
			Type:     TUnknown,
			Args:     [3]ValID{rk.arrVal, rk.baseOff, NoVal},
			NumArgs:  numArgs,
			UpvalIdx: -1,
			ImmI:     sp.minDelta,
			ImmI2:    sp.maxDelta,
		})
		for _, p := range sp.instPtrs {
			p.BCE = true
		}
	}
}

func runDCE(reg *LoopRegion) {
	used := make([]bool, reg.NextVal)
	mark := func(v ValID) {
		if v >= 0 && int(v) < len(used) {
			used[v] = true
		}
	}
	for _, lv := range reg.Vars {
		mark(lv.InitVal)
	}
	// Fixed-point backward liveness over body blocks and preheader.
	for pass := 0; pass < 4; pass++ {
		changed := false
		markCh := func(v ValID) {
			if v >= 0 && int(v) < len(used) && !used[v] {
				used[v] = true
				changed = true
			}
		}
		for _, blk := range reg.Blocks {
			if blk.Term.CondVal != NoVal {
				markCh(blk.Term.CondVal)
			}
			for i := len(blk.Insts) - 1; i >= 0; i-- {
				in := &blk.Insts[i]
				if in.Dead {
					continue
				}
				if hasSideEffect(in.Op) || used[in.ID] {
					for k := uint8(0); k < in.NumArgs; k++ {
						markCh(in.Args[k])
					}
				}
			}
		}
		for i := len(reg.Preheader) - 1; i >= 0; i-- {
			in := &reg.Preheader[i]
			if in.Dead {
				continue
			}
			if hasSideEffect(in.Op) || used[in.ID] {
				for k := uint8(0); k < in.NumArgs; k++ {
					markCh(in.Args[k])
				}
			}
		}
		if !changed {
			break
		}
	}
	for _, blk := range reg.Blocks {
		for i := range blk.Insts {
			in := &blk.Insts[i]
			if !in.Dead && !hasSideEffect(in.Op) && !used[in.ID] {
				in.Dead = true
			}
		}
	}
	for i := range reg.Preheader {
		in := &reg.Preheader[i]
		if !in.Dead && !hasSideEffect(in.Op) && !used[in.ID] {
			in.Dead = true
		}
	}
}

func hasSideEffect(op Op) bool {
	switch op {
	case OpLoopCnt, OpLoopStep, OpLoopIdx,
		OpSetVar,
		OpStoreArrInt, OpStoreArrFlt, OpStoreArrBool,
		OpStoreSlotFloat, OpStoreSlotInt,
		OpGuardRange, OpGuardShape, OpLoadArrTable,
		OpGuardUpvalSqrt, OpGuardUpvalAbs, OpGuardRegSqrt, OpGuardRegAbs,
		OpGuardCalleePro, OpIDivI, OpModI,
		OpLoadSlotFloat, OpLoadSlotInt:
		return true
	}
	return false
}

func rewriteArgs(in *Inst, remap map[ValID]ValID) {
	if len(remap) == 0 {
		return
	}
	for k := uint8(0); k < in.NumArgs; k++ {
		if r, ok := remap[in.Args[k]]; ok {
			in.Args[k] = r
		}
	}
}

func applyRemap(reg *LoopRegion, remap map[ValID]ValID) {
	for i := range reg.Preheader {
		rewriteArgs(&reg.Preheader[i], remap)
	}
	for i := range reg.Vars {
		if r, ok := remap[reg.Vars[i].InitVal]; ok {
			reg.Vars[i].InitVal = r
		}
	}
	for _, blk := range reg.Blocks {
		for i := range blk.Insts {
			rewriteArgs(&blk.Insts[i], remap)
		}
		if r, ok := remap[blk.Term.CondVal]; ok {
			blk.Term.CondVal = r
		}
	}
	// Also update Affine map BaseOff references.
	for id, af := range reg.Affine {
		if af.BaseOff != NoVal {
			if r, ok := remap[af.BaseOff]; ok {
				af.BaseOff = r
				reg.Affine[id] = af
			}
		}
	}
}
