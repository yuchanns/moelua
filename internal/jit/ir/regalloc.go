package ir

// AllocateRegisters performs fast linear-scan register allocation over reg for a target
// with numGPR allocatable scratch GPRs, numFP allocatable FP/SIMD registers, and up to
// maxSpills 64-bit spill slots in jitExecCtx.optSpill.
//
// Allocation policy:
//   - Mutable LoopVars and Preheader hoisted values (arrBase, constants, invariants) get first
//     priority for dedicated hardware registers so the inner loop has near-zero spills.
//   - Per-block SSA temporaries use live-interval linear scan, releasing registers as soon as
//     their last use passes.
//   - Returns nil if even with maxSpills the loop exceeds register + spill capacity.
func AllocateRegisters(reg *LoopRegion, numGPR, numFP, maxSpills int) *AllocResult {
	if reg == nil {
		return nil
	}
	nVals := int(reg.NextVal)
	res := &AllocResult{
		ValLoc: make([]RegLoc, nVals),
		VarLoc: make([]RegLoc, len(reg.Vars)),
		LenLoc: make([]RegLoc, nVals),
	}
	for i := range res.ValLoc {
		res.ValLoc[i] = RegLoc{Phys: -1, Spill: -1}
		res.LenLoc[i] = RegLoc{Phys: -1, Spill: -1}
	}
	for i := range res.VarLoc {
		res.VarLoc[i] = RegLoc{Phys: -1, Spill: -1}
	}

	// Track which hardware registers are permanently pinned across the entire loop (vars + preheader).
	gprPinned := make([]bool, numGPR)
	fpPinned := make([]bool, numFP)
	nextSpill := 0

	allocPinned := func(isFP bool) (RegLoc, bool) {
		if isFP {
			for r := 0; r < numFP; r++ {
				if !fpPinned[r] {
					fpPinned[r] = true
					return RegLoc{IsFP: true, Phys: int8(r), Spill: -1}, true
				}
			}
		} else {
			for r := 0; r < numGPR; r++ {
				if !gprPinned[r] {
					gprPinned[r] = true
					return RegLoc{IsFP: false, Phys: int8(r), Spill: -1}, true
				}
			}
		}
		if nextSpill >= maxSpills {
			return RegLoc{}, false
		}
		sl := int8(nextSpill)
		nextSpill++
		return RegLoc{IsFP: isFP, Phys: -1, Spill: sl}, true
	}

	// 1. Reserve LoopCnt (for LoopForInt) — wait: LoopCnt & LoopStep are kept in dedicated Preheader vals
	// or spill slots; let's allocate LoopVars FIRST so the hottest inner-loop accumulators and index
	// ALWAYS win hardware registers!
	for i, lv := range reg.Vars {
		loc, ok := allocPinned(lv.Type.IsFPReg())
		if !ok {
			return nil
		}
		res.VarLoc[i] = loc
	}

	// 2. Identify which preheader values are actually used inside the loop body (vs preheader-only).
	usedInBody := make([]bool, nVals)
	needsDynLen := make([]bool, nVals)
	for _, blk := range reg.Blocks {
		if blk.Term.CondVal != NoVal {
			usedInBody[blk.Term.CondVal] = true
		}
		for i := range blk.Insts {
			in := &blk.Insts[i]
			if in.Dead {
				continue
			}
			for k := uint8(0); k < in.NumArgs; k++ {
				if in.Args[k] != NoVal {
					usedInBody[in.Args[k]] = true
				}
			}
			// Un-BCE'd array accesses need the hoisted arrLen in body!
			switch in.Op {
			case OpLoadArrInt, OpLoadArrFloat, OpLoadArrBool, OpLoadArrTable,
				OpStoreArrInt, OpStoreArrFlt, OpStoreArrBool:
				if !in.BCE && in.Args[0] != NoVal {
					needsDynLen[in.Args[0]] = true
				}
			}
		}
	}
	// Also mark arrLen needed for every preheader OpGuardRange!
	for i := range reg.Preheader {
		in := &reg.Preheader[i]
		if !in.Dead && in.Op == OpGuardRange && in.Args[0] != NoVal {
			needsDynLen[in.Args[0]] = true
		}
	}
	if reg.Vec != nil {
		for _, sp := range reg.Vec.Splats {
			if sp.ScalarVal != NoVal {
				usedInBody[sp.ScalarVal] = true
			}
		}
		for _, p := range reg.Vec.Ptrs {
			usedInBody[p.ArrVal] = true
			if p.BaseOff != NoVal {
				usedInBody[p.BaseOff] = true
			}
		}
	}

	// Allocate pinned registers/spills for preheader values needed inside the body or loop control.
	// Always keep at least 2 free GPRs and 2 free FP regs for body temporaries & emitter scratch!
	const minBodyGPR = 3
	const minBodyFP = 3
	countFree := func(pinned []bool) int {
		n := 0
		for _, p := range pinned {
			if !p {
				n++
			}
		}
		return n
	}
	allocPre := func(isFP bool) (RegLoc, bool) {
		if isFP {
			if countFree(fpPinned) > minBodyFP {
				return allocPinned(true)
			}
		} else {
			if countFree(gprPinned) > minBodyGPR {
				return allocPinned(false)
			}
		}
		if nextSpill >= maxSpills {
			return RegLoc{}, false
		}
		sl := int8(nextSpill)
		nextSpill++
		return RegLoc{IsFP: isFP, Phys: -1, Spill: sl}, true
	}

	// LoopCnt & LoopStep: allocate first (LoopCnt is decremented on every loop iteration!).
	for i := range reg.Preheader {
		in := &reg.Preheader[i]
		if in.Dead {
			continue
		}
		switch in.Op {
		case OpLoopCnt:
			loc, ok := allocPinned(false)
			if !ok {
				return nil
			}
			res.ValLoc[in.ID] = loc
		case OpLoopStep:
			if !reg.HasConstStep {
				loc, ok := allocPre(false)
				if !ok {
					return nil
				}
				res.ValLoc[in.ID] = loc
			}
		}
	}

	// Remaining preheader values used inside the body (or needed by OpGuardRange/Vars init).
	// Allocate hoisted array bases first, then invariants/constants.
	for _, passArr := range []bool{true, false} {
		for i := range reg.Preheader {
			in := &reg.Preheader[i]
			if in.Dead || res.ValLoc[in.ID].Phys >= 0 || res.ValLoc[in.ID].Spill >= 0 {
				continue
			}
			if in.Op == OpLoopIdx || in.Op == OpLoopCnt || in.Op == OpLoopStep ||
				in.Op == OpGuardRange || in.Op == OpGuardUpvalSqrt || in.Op == OpGuardUpvalAbs ||
				in.Op == OpGuardRegSqrt || in.Op == OpGuardRegAbs || in.Op == OpGuardCalleePro ||
				in.Op == OpLoadArrLen {
				continue
			}
			isArr := in.Op == OpGuardTableArr
			if isArr != passArr {
				continue
			}
			// Preheader values only used by LoopVar InitVal don't need permanent slots unless also used in body
			// or by OpGuardRange.
			usedPre := false
			for j := i + 1; j < len(reg.Preheader); j++ {
				pj := &reg.Preheader[j]
				if pj.Dead {
					continue
				}
				for k := uint8(0); k < pj.NumArgs; k++ {
					if pj.Args[k] == in.ID {
						usedPre = true
					}
				}
			}
			for _, lv := range reg.Vars {
				if lv.InitVal == in.ID {
					usedPre = true
				}
			}
			if !usedInBody[in.ID] && !usedPre {
				continue
			}
			loc, ok := allocPre(in.Type.IsFPReg())
			if !ok {
				return nil
			}
			res.ValLoc[in.ID] = loc
			if isArr && needsDynLen[in.ID] {
				lloc, ok := allocPre(false)
				if !ok {
					return nil
				}
				res.LenLoc[in.ID] = lloc
			}
		}
	}

	// Also allocate OpLoadArrLen if used by OpGuardRange or body.
	for i := range reg.Preheader {
		in := &reg.Preheader[i]
		if in.Dead || in.Op != OpLoadArrLen {
			continue
		}
		if res.LenLoc[in.Args[0]].Phys >= 0 || res.LenLoc[in.Args[0]].Spill >= 0 {
			res.ValLoc[in.ID] = res.LenLoc[in.Args[0]]
			continue
		}
		// Only allocate if referenced in body or preheader.
		loc, ok := allocPre(false)
		if !ok {
			return nil
		}
		res.ValLoc[in.ID] = loc
		res.LenLoc[in.Args[0]] = loc
	}

	// 3. Per-block linear-scan allocation for body SSA temporaries using the remaining unpinned regs!
	for _, blk := range reg.Blocks {
		// Compute last use index within this block for each body-defined ValID.
		lastUse := make(map[ValID]int, len(blk.Insts))
		for idx := range blk.Insts {
			in := &blk.Insts[idx]
			if in.Dead {
				continue
			}
			for k := uint8(0); k < in.NumArgs; k++ {
				if a := in.Args[k]; a != NoVal {
					lastUse[a] = idx
				}
			}
		}
		if blk.Term.CondVal != NoVal {
			lastUse[blk.Term.CondVal] = len(blk.Insts)
		}

		gprBusy := make([]bool, numGPR)
		copy(gprBusy, gprPinned)
		fpBusy := make([]bool, numFP)
		copy(fpBusy, fpPinned)

		active := make(map[ValID]RegLoc, 8)
		freeDead := func(curIdx int) {
			for id, loc := range active {
				if lastUse[id] <= curIdx {
					if loc.Phys >= 0 {
						if loc.IsFP {
							fpBusy[loc.Phys] = false
						} else {
							gprBusy[loc.Phys] = false
						}
					}
					delete(active, id)
				}
			}
		}

		for idx := range blk.Insts {
			in := &blk.Insts[idx]
			if in.Dead {
				continue
			}
			// Release registers whose last use was strictly BEFORE this instruction (or at this instruction
			// after reading args; to keep emitter simple without dst==src clobber hazards on 2-addr ops,
			// free BEFORE allocating only for values whose lastUse < idx!).
			freeDead(idx - 1)

			// Side-effect-only instructions don't define a value.
			if in.Type == TUnknown || in.Op == OpSetVar ||
				in.Op == OpStoreArrInt || in.Op == OpStoreArrFlt || in.Op == OpStoreArrBool ||
				in.Op == OpStoreSlotFloat || in.Op == OpStoreSlotInt {
				continue
			}
			// OpGetVar for a LoopVar: alias directly to the LoopVar's pinned RegLoc if the LoopVar is
			// not overwritten before this OpGetVar's last use! That eliminates redundant register copies!
			if in.Op == OpGetVar {
				vi := int(in.AuxIdx)
				lu := lastUse[in.ID]
				overwritten := false
				for j := idx + 1; j < lu && j < len(blk.Insts); j++ {
					if !blk.Insts[j].Dead && blk.Insts[j].Op == OpSetVar && int(blk.Insts[j].AuxIdx) == vi {
						overwritten = true
						break
					}
				}
				if !overwritten {
					res.ValLoc[in.ID] = res.VarLoc[vi]
					continue
				}
			}

			isFP := in.Type.IsFPReg()
			loc := RegLoc{IsFP: isFP, Phys: -1, Spill: -1}
			if isFP {
				for r := 0; r < numFP; r++ {
					if !fpBusy[r] {
						fpBusy[r] = true
						loc.Phys = int8(r)
						break
					}
				}
			} else {
				for r := 0; r < numGPR; r++ {
					if !gprBusy[r] {
						gprBusy[r] = true
						loc.Phys = int8(r)
						break
					}
				}
			}
			if loc.Phys < 0 {
				if nextSpill >= maxSpills {
					return nil
				}
				loc.Spill = int8(nextSpill)
				nextSpill++
			}
			res.ValLoc[in.ID] = loc
			if lastUse[in.ID] > idx {
				active[in.ID] = loc
			} else if loc.Phys >= 0 {
				// Unused or immediately dead: free right away.
				if loc.IsFP {
					fpBusy[loc.Phys] = false
				} else {
					gprBusy[loc.Phys] = false
				}
			}
		}
	}

	res.UsedSpills = nextSpill
	return res
}
