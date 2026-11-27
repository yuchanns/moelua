package ir

// VectorizeRegion analyzes a single-block counted stride-1 LoopRegion (`step == 1`) and,
// when proven dependency-safe and vectorizable, populates reg.Vec with a 2-wide (128-bit)
// SIMD VecPlan (ARM64 NEON .2D / AMD64 SSE2 PD+PQ) plus preheader alias guards.
//
// Supported general patterns (not hardcoded templates):
//   - Elementwise float64 & int64 array kernels: DAXPY (`C[rowA+j] = C[rowA+j] + aik * B[rowB+j]`),
//     LU elimination (`vi[k] = vi[k] - eij * vj[k]`), scaling (`v[i] = v[i] * norm`),
//     vector copy (`vd[x] = vs[x]`), vector add/sub/mul/div/sqrt/abs/bitwise.
//   - Exact reductions:
//   - float64 additive reductions (`vBv = vBv + u[i]*v[i]`, `sum = sum + C[i]`) using SIMD
//     loads + SIMD arithmetic + strict left-to-right ordered lane0/lane1 scalar accumulation
//     for 100% bit-for-bit IEEE-754 parity with scalar Lua 5.5.1.
//   - int64 additive reductions (`sum = sum + a[i]`) using associative 2-wide int64 vector
//     accumulators folded horizontally after the vector loop.
//
// Any loop with odd tail iterations finishes cleanly in the scalar optimized loop epilogue.
func VectorizeRegion(reg *LoopRegion) {
	if reg == nil || reg.Kind != LoopForInt || len(reg.Blocks) != 1 {
		return
	}
	blk := reg.Blocks[0]
	if blk.Term.Kind != TermBackedge {
		return
	}
	// Require stride +1 (either statically known ConstStep == 1, or checked dynamically in preheader
	// when ConstStep is unknown — only vectorize when HasConstStep && ConstStep == 1 so we don't
	// emit unused SIMD code for step == -1 or step == 2 loops).
	if !reg.HasConstStep || reg.ConstStep != 1 {
		return
	}

	// Build quick lookup of preheader invariant values & LoopVar usage.
	preInst := make(map[ValID]*Inst, len(reg.Preheader))
	for i := range reg.Preheader {
		if !reg.Preheader[i].Dead {
			preInst[reg.Preheader[i].ID] = &reg.Preheader[i]
		}
	}

	// Identify which LoopVars are written in the body: only the loop index (IsForIdx) and
	// additive reduction accumulators (`v = v + vecExpr`) are allowed in a vectorized loop!
	varWrites := make(map[int]int)
	for i := range blk.Insts {
		in := &blk.Insts[i]
		if in.Dead {
			continue
		}
		if in.Op == OpSetVar {
			varWrites[int(in.AuxIdx)]++
		}
	}
	for vi, cnt := range varWrites {
		if cnt > 1 {
			// Multiple updates to same mutable scalar in one iteration -> stay scalar.
			return
		}
		lv := reg.Vars[vi]
		if lv.IsForIdx {
			return // should never be SetVar'd
		}
		if lv.Type != TFloat && lv.Type != TInt {
			return
		}
	}

	// Collect all memory reads and writes to check contiguity (BCE + affine stride +1) and aliasing.
	type memAccess struct {
		inst    *Inst
		arrVal  ValID
		baseOff ValID
		delta   int64
		isFloat bool
		isWrite bool
	}
	var accesses []memAccess
	hasMemOp := false
	for i := range blk.Insts {
		in := &blk.Insts[i]
		if in.Dead {
			continue
		}
		switch in.Op {
		case OpLoadArrFloat, OpLoadArrInt, OpStoreArrFlt, OpStoreArrInt:
			if !in.BCE {
				return
			}
			af, ok := reg.Affine[in.Args[1]]
			if !ok || !af.Valid {
				return
			}
			isF := in.Op == OpLoadArrFloat || in.Op == OpStoreArrFlt
			isW := in.Op == OpStoreArrFlt || in.Op == OpStoreArrInt
			accesses = append(accesses, memAccess{
				inst:    in,
				arrVal:  in.Args[0],
				baseOff: af.BaseOff,
				delta:   af.Delta,
				isFloat: isF,
				isWrite: isW,
			})
			hasMemOp = true
		case OpLoadArrBool, OpStoreArrBool, OpLoadArrTable,
			OpGuardShape, OpLoadSlotFloat, OpLoadSlotInt, OpStoreSlotFloat, OpStoreSlotInt:
			return
		}
	}
	if !hasMemOp {
		// Pure scalar loop without contiguous array loads/stores (e.g. forloop_int) doesn't benefit from 2-wide array SIMD.
		return
	}

	// Dependency & alias safety check:
	// - Same-array read & write with DIFFERENT delta (e.g. SOR's v[x-1], v[x+1] while writing v[x])
	//   has a loop-carried dependence -> do NOT vectorize!
	// - Distinct-array (or same-array with dynamic BaseOff) write vs read/write: emit runtime AliasCheck!
	var aliasChecks []AliasCheck
	type aliasPairKey struct {
		a1, o1 ValID
		d1     int64
		a2, o2 ValID
		d2     int64
	}
	seenAlias := make(map[aliasPairKey]bool)

	for _, w := range accesses {
		if !w.isWrite {
			continue
		}
		for _, o := range accesses {
			if w.inst == o.inst {
				continue
			}
			// Exact same array, baseOff, and delta -> elementwise read-modify-write (e.g. C[rowA+j] += ...) is ALWAYS safe!
			if w.arrVal == o.arrVal && w.baseOff == o.baseOff && w.delta == o.delta {
				continue
			}
			// Same array and same baseOff, DIFFERENT delta (e.g. v[x] = ... v[x-1] ...) -> definite loop-carried overlap!
			if w.arrVal == o.arrVal && w.baseOff == o.baseOff && w.delta != o.delta {
				return
			}
			// Otherwise (different table vars that could alias at runtime, or same table with different baseOff
			// like C[rowA+j] vs C[rowB+j]): add runtime non-overlap alias guard!
			pk := aliasPairKey{w.arrVal, w.baseOff, w.delta, o.arrVal, o.baseOff, o.delta}
			if !seenAlias[pk] {
				seenAlias[pk] = true
				aliasChecks = append(aliasChecks, AliasCheck{
					DstArr: w.arrVal, DstOff: w.baseOff, DstDel: w.delta,
					SrcArr: o.arrVal, SrcOff: o.baseOff, SrcDel: o.delta,
				})
			}
		}
	}

	// Build VecPlan by walking blk.Insts in order.
	plan := &VecPlan{
		Width:       2,
		AliasChecks: aliasChecks,
	}

	// Map each unique (arrVal, baseOff, delta, isFloat) stream to a single VecPtr.
	type ptrKey struct {
		arrVal  ValID
		baseOff ValID
		delta   int64
		isFloat bool
	}
	ptrMap := make(map[ptrKey]int, len(accesses))
	getPtr := func(arrVal, baseOff ValID, delta int64, isFloat, isWrite bool) int {
		pk := ptrKey{arrVal, baseOff, delta, isFloat}
		if idx, ok := ptrMap[pk]; ok {
			if isWrite {
				plan.Ptrs[idx].IsWrite = true
			}
			return idx
		}
		idx := len(plan.Ptrs)
		plan.Ptrs = append(plan.Ptrs, VecPtr{
			ArrVal:  arrVal,
			BaseOff: baseOff,
			Delta:   delta,
			IsFloat: isFloat,
			IsWrite: isWrite,
			PtrIdx:  idx,
		})
		ptrMap[pk] = idx
		return idx
	}

	// Map loop-invariant scalar ValID -> splatted virtual vector reg.
	splatMap := make(map[ValID]int, 8)
	valVReg := make(map[ValID]int, len(blk.Insts))
	// Track OpGetVar results for reduction accumulators: valID -> varIdx.
	varGetID := make(map[ValID]int, 4)
	// Map int64 reduction varIdx -> IntAccums index.
	intAccMap := make(map[int]int, 4)

	newVReg := func() int {
		r := plan.NumVRegs
		plan.NumVRegs++
		return r
	}

	getVecOperand := func(id ValID) (int, bool) {
		if vr, ok := valVReg[id]; ok {
			return vr, true
		}
		if pi, ok := preInst[id]; ok {
			if pi.Type != TFloat && pi.Type != TInt {
				return -1, false
			}
			if pi.Op == OpLoopIdx || pi.Op == OpLoopCnt {
				return -1, false
			}
			if vr, exists := splatMap[id]; exists {
				return vr, true
			}
			vr := newVReg()
			splatMap[id] = vr
			sp := VecSplat{
				ScalarVal: id,
				IsFloat:   pi.Type == TFloat,
				VReg:      vr,
			}
			if pi.Op == OpConstFloat {
				sp.IsConst = true
				sp.ConstBits = pi.ImmU
			} else if pi.Op == OpConstInt {
				sp.IsConst = true
				sp.ConstBits = uint64(pi.ImmI)
			}
			plan.Splats = append(plan.Splats, sp)
			return vr, true
		}
		return -1, false
	}

	// Track which SSA values are solely used as affine index calculations (ignored by SIMD body since
	// VecPtr advances directly by +16 bytes per SIMD iteration!).
	idxUsedOnlyForMem := make(map[ValID]bool, len(blk.Insts))
	for id, af := range reg.Affine {
		if af.Valid {
			idxUsedOnlyForMem[id] = true
		}
	}
	// Verify no affine index value is consumed by a non-index arithmetic op (e.g. `a[i] = i * 2`).
	for i := range blk.Insts {
		in := &blk.Insts[i]
		if in.Dead {
			continue
		}
		switch in.Op {
		case OpLoadArrFloat, OpLoadArrInt:
			// Args[1] is index -> OK.
		case OpStoreArrFlt, OpStoreArrInt:
			// Args[1] is index -> OK; Args[2] is value -> if affine, cannot vectorize simply.
			if idxUsedOnlyForMem[in.Args[2]] {
				return
			}
		case OpAddI, OpAddIImm:
			if idxUsedOnlyForMem[in.ID] {
				continue // part of affine address computation
			}
			for k := uint8(0); k < in.NumArgs; k++ {
				if idxUsedOnlyForMem[in.Args[k]] {
					return
				}
			}
		default:
			for k := uint8(0); k < in.NumArgs; k++ {
				if idxUsedOnlyForMem[in.Args[k]] {
					return
				}
			}
		}
	}

	// Track pending reduction expressions (`acc = acc + expr` or `acc = expr + acc`).
	type pendingRed struct {
		varIdx  int
		exprVR  int
		isFloat bool
	}
	redExpr := make(map[ValID]pendingRed, 4)

	for i := range blk.Insts {
		in := &blk.Insts[i]
		if in.Dead {
			continue
		}
		// Skip pure affine index computations (handled by advancing VecPtrs).
		if idxUsedOnlyForMem[in.ID] {
			continue
		}
		switch in.Op {
		case OpGetVar:
			vi := int(in.AuxIdx)
			if reg.Vars[vi].IsForIdx {
				continue
			}
			if varWrites[vi] == 0 {
				// Read-only LoopVar: cannot happen normally, bail safely.
				return
			}
			varGetID[in.ID] = vi

		case OpConstFloat, OpConstInt:
			// Body-local constant: materialize directly from immediate bits in SIMD preheader!
			vr := newVReg()
			bits := uint64(in.ImmI)
			if in.Op == OpConstFloat {
				bits = in.ImmU
			}
			plan.Splats = append(plan.Splats, VecSplat{
				ScalarVal: in.ID,
				IsFloat:   in.Type == TFloat,
				IsConst:   true,
				ConstBits: bits,
				VReg:      vr,
			})
			valVReg[in.ID] = vr

		case OpLoadArrFloat:
			af := reg.Affine[in.Args[1]]
			pi := getPtr(in.Args[0], af.BaseOff, af.Delta, true, false)
			vr := newVReg()
			plan.Body = append(plan.Body, VecInst{Op: VLoadF64, Dst: vr, Src1: pi})
			valVReg[in.ID] = vr

		case OpLoadArrInt:
			af := reg.Affine[in.Args[1]]
			pi := getPtr(in.Args[0], af.BaseOff, af.Delta, false, false)
			vr := newVReg()
			plan.Body = append(plan.Body, VecInst{Op: VLoadI64, Dst: vr, Src1: pi})
			valVReg[in.ID] = vr

		case OpStoreArrFlt:
			af := reg.Affine[in.Args[1]]
			pi := getPtr(in.Args[0], af.BaseOff, af.Delta, true, true)
			valVR, ok := getVecOperand(in.Args[2])
			if !ok {
				return
			}
			plan.Body = append(plan.Body, VecInst{Op: VStoreF64, Dst: -1, Src1: pi, Src2: valVR})

		case OpStoreArrInt:
			af := reg.Affine[in.Args[1]]
			pi := getPtr(in.Args[0], af.BaseOff, af.Delta, false, true)
			valVR, ok := getVecOperand(in.Args[2])
			if !ok {
				return
			}
			plan.Body = append(plan.Body, VecInst{Op: VStoreI64, Dst: -1, Src1: pi, Src2: valVR})

		case OpAddF:
			// Check if this is an additive FP64 reduction: `acc + vecExpr` or `vecExpr + acc`.
			if vi, isAcc := varGetID[in.Args[0]]; isAcc {
				evr, ok := getVecOperand(in.Args[1])
				if !ok {
					return
				}
				redExpr[in.ID] = pendingRed{varIdx: vi, exprVR: evr, isFloat: true}
				continue
			}
			if vi, isAcc := varGetID[in.Args[1]]; isAcc {
				evr, ok := getVecOperand(in.Args[0])
				if !ok {
					return
				}
				redExpr[in.ID] = pendingRed{varIdx: vi, exprVR: evr, isFloat: true}
				continue
			}
			s1, ok1 := getVecOperand(in.Args[0])
			s2, ok2 := getVecOperand(in.Args[1])
			if !ok1 || !ok2 {
				return
			}
			vr := newVReg()
			plan.Body = append(plan.Body, VecInst{Op: VAddF64, Dst: vr, Src1: s1, Src2: s2})
			valVReg[in.ID] = vr

		case OpAddI:
			if vi, isAcc := varGetID[in.Args[0]]; isAcc {
				evr, ok := getVecOperand(in.Args[1])
				if !ok {
					return
				}
				redExpr[in.ID] = pendingRed{varIdx: vi, exprVR: evr, isFloat: false}
				continue
			}
			if vi, isAcc := varGetID[in.Args[1]]; isAcc {
				evr, ok := getVecOperand(in.Args[0])
				if !ok {
					return
				}
				redExpr[in.ID] = pendingRed{varIdx: vi, exprVR: evr, isFloat: false}
				continue
			}
			s1, ok1 := getVecOperand(in.Args[0])
			s2, ok2 := getVecOperand(in.Args[1])
			if !ok1 || !ok2 {
				return
			}
			vr := newVReg()
			plan.Body = append(plan.Body, VecInst{Op: VAddI64, Dst: vr, Src1: s1, Src2: s2})
			valVReg[in.ID] = vr

		case OpSubF, OpMulF, OpDivF, OpSubI, OpAndI, OpOrI, OpXorI:
			s1, ok1 := getVecOperand(in.Args[0])
			s2, ok2 := getVecOperand(in.Args[1])
			if !ok1 || !ok2 {
				return
			}
			var vop VecOp
			switch in.Op {
			case OpSubF:
				vop = VSubF64
			case OpMulF:
				vop = VMulF64
			case OpDivF:
				vop = VDivF64
			case OpSubI:
				vop = VSubI64
			case OpAndI:
				vop = VAndI64
			case OpOrI:
				vop = VOrI64
			case OpXorI:
				vop = VXorI64
			}
			vr := newVReg()
			plan.Body = append(plan.Body, VecInst{Op: vop, Dst: vr, Src1: s1, Src2: s2})
			valVReg[in.ID] = vr

		case OpNegF, OpSqrtF, OpAbsF:
			s1, ok := getVecOperand(in.Args[0])
			if !ok {
				return
			}
			var vop VecOp
			switch in.Op {
			case OpNegF:
				vop = VNegF64
			case OpSqrtF:
				vop = VSqrtF64
			case OpAbsF:
				vop = VAbsF64
			}
			vr := newVReg()
			plan.Body = append(plan.Body, VecInst{Op: vop, Dst: vr, Src1: s1})
			valVReg[in.ID] = vr

		case OpSetVar:
			vi := int(in.AuxIdx)
			pr, ok := redExpr[in.Args[0]]
			if !ok || pr.varIdx != vi {
				return
			}
			if pr.isFloat {
				plan.Body = append(plan.Body, VecInst{
					Op:     VAccumF64,
					Dst:    -1,
					Src1:   pr.exprVR,
					VarIdx: vi,
				})
			} else {
				accIdx, exists := intAccMap[vi]
				if !exists {
					accIdx = len(plan.IntAccums)
					accVR := newVReg()
					plan.IntAccums = append(plan.IntAccums, VecIntAccum{VarIdx: vi, VReg: accVR})
					intAccMap[vi] = accIdx
				}
				plan.Body = append(plan.Body, VecInst{
					Op:     VAccumI64,
					Dst:    plan.IntAccums[accIdx].VReg,
					Src1:   pr.exprVR,
					VarIdx: vi,
					AccIdx: accIdx,
				})
			}

		default:
			// Unsupported op inside SIMD body (e.g. OpMulI, OpIDivI, OpModI, OpShlI, OpCmp) -> stay scalar.
			return
		}
	}

	// Keep vector register pressure reasonable so all vector regs stay in hardware V/XMM registers.
	if len(plan.Body) == 0 || plan.NumVRegs > 12 || len(plan.Ptrs) > 4 {
		return
	}
	reg.Vec = plan
}
