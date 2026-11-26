package compiler

import (
	"math"

	"github.com/yuchanns/moelua/bytecode"
)

const (
	noJump     = -1
	noReg      = bytecode.MaxArgA // 255
	maxFStack  = bytecode.MaxArgA // 255
	maxIndexRK = bytecode.MaxArgB // 255
	luaMultRet = -1
	maxVars    = 200
	maxUpval   = 255
	maxCnst    = math.MaxInt32 / 2

	absLineInfo = -0x80 // int8(-128)
	limLineDiff = 0x80  // 128
	maxIWthAbs  = 128
)

// Expression kinds matching PUC-Rio Lua 5.5.1 lparser.h.
type expKind uint8

const (
	vVoid expKind = iota
	vNil
	vTrue
	vFalse
	vK
	vKFlt
	vKInt
	vKStr
	vNonReloc
	vLocal
	vVargVar
	vGlobal
	vUpval
	vConst
	vIndexed
	vVargInd
	vIndexUp
	vIndexI
	vIndexStr
	vJmp
	vReloc
	vCall
	vVararg
)

func (k expKind) isVar() bool     { return vLocal <= k && k <= vIndexStr }
func (k expKind) isIndexed() bool { return vIndexed <= k && k <= vIndexStr }
func (k expKind) hasMultRet() bool {
	return k == vCall || k == vVararg
}

// Variable kinds matching PUC-Rio Lua 5.5.1 lparser.h.
const (
	vdkReg     uint8 = 0 // regular local
	rdkConst   uint8 = 1 // local constant
	rdkVaVar   uint8 = 2 // vararg parameter
	rdkToClose uint8 = 3 // to-be-closed
	rdkCTC     uint8 = 4 // local compile-time constant
	gdkReg     uint8 = 5 // regular global
	gdkConst   uint8 = 6 // global constant
)

type expDesc struct {
	k      expKind
	ival   int64
	nval   float64
	strval string
	info   int
	ind    struct {
		idx    int16
		t      uint8
		ro     uint8
		keystr int
	}
	varInfo struct {
		ridx uint8
		vidx int16
	}
	t int // patch list of 'exit when true'
	f int // patch list of 'exit when false'
}

func (e *expDesc) hasJumps() bool { return e.t != e.f }

func initExp(e *expDesc, k expKind, i int) {
	e.f = noJump
	e.t = noJump
	e.k = k
	e.info = i
}

func codeString(e *expDesc, s string) {
	e.f = noJump
	e.t = noJump
	e.k = vKStr
	e.strval = s
}

type varDesc struct {
	kind    uint8
	ridx    uint8
	pidx    int16
	name    string
	hasName bool // false for collective 'global *' entries
	kVal    bytecode.Constant
}

func (v *varDesc) inReg() bool    { return v.kind <= rdkToClose }
func (v *varDesc) isGlobal() bool { return v.kind >= gdkReg }

type labelDesc struct {
	name    string
	pc      int
	line    int
	nactvar int16
	close   bool
}

type dynData struct {
	actvar  []varDesc
	actvarN int
	gt      []labelDesc
	label   []labelDesc
}

// Binary operators matching PUC-Rio lcode.h (ORDER OPR - ORDER OP - ORDER TM).
type binOpr uint8

const (
	oprAdd binOpr = iota
	oprSub
	oprMul
	oprMod
	oprPow
	oprDiv
	oprIDiv
	oprBAnd
	oprBOr
	oprBXor
	oprShl
	oprShr
	oprConcat
	oprEQ
	oprLT
	oprLE
	oprNE
	oprGT
	oprGE
	oprAnd
	oprOr
	oprNoBinOpr
)

func (op binOpr) foldable() bool { return op <= oprShr }

// Unary operators matching PUC-Rio lcode.h (ORDER OPR - ORDER OP).
type unOpr uint8

const (
	oprMinus unOpr = iota
	oprBNot
	oprNot
	oprLen
	oprNoUnOpr
)

// Arithmetic opcodes for constant folding matching lua.h (LUA_OPADD..LUA_OPBNOT).
const (
	luaOpAdd  = 0
	luaOpSub  = 1
	luaOpMul  = 2
	luaOpMod  = 3
	luaOpPow  = 4
	luaOpDiv  = 5
	luaOpIDiv = 6
	luaOpBAnd = 7
	luaOpBOr  = 8
	luaOpBXor = 9
	luaOpShl  = 10
	luaOpShr  = 11
	luaOpUnm  = 12
	luaOpBNot = 13
)

// Tag method indices matching ltm.h (ORDER TM).
type tmEvent uint8

const (
	tmIndex tmEvent = iota
	tmNewIndex
	tmGC
	tmMode
	tmLen
	tmEQ
	tmAdd
	tmSub
	tmMul
	tmMod
	tmPow
	tmDiv
	tmIDiv
	tmBAnd
	tmBOr
	tmBXor
	tmShl
	tmShr
	tmUnm
	tmBNot
	tmLT
	tmLE
	tmConcat
	tmCall
	tmClose
)

func binOpr2Op(opr, baser binOpr, base bytecode.OpCode) bytecode.OpCode {
	return bytecode.OpCode(int(opr) - int(baser) + int(base))
}

func unOpr2Op(opr unOpr) bytecode.OpCode {
	return bytecode.OpCode(int(opr) - int(oprMinus) + int(bytecode.OP_UNM))
}

func binOpr2TM(opr binOpr) tmEvent {
	return tmEvent(int(opr) - int(oprAdd) + int(tmAdd))
}

// Instruction encoding and mutation helpers matching lopcodes.h.
type opMode uint8

const (
	modeABC opMode = iota
	modevABC
	modeABx
	modeAsBx
	modeAx
	modeSJ
)

func packOpMode(mm, ot, it, t, a uint8, m opMode) uint8 {
	return (mm << 7) | (ot << 6) | (it << 5) | (t << 4) | (a << 3) | uint8(m)
}

var opModes = [bytecode.NumOpcodes]uint8{
	bytecode.OP_MOVE:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_LOADI:      packOpMode(0, 0, 0, 0, 1, modeAsBx),
	bytecode.OP_LOADF:      packOpMode(0, 0, 0, 0, 1, modeAsBx),
	bytecode.OP_LOADK:      packOpMode(0, 0, 0, 0, 1, modeABx),
	bytecode.OP_LOADKX:     packOpMode(0, 0, 0, 0, 1, modeABx),
	bytecode.OP_LOADFALSE:  packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_LFALSESKIP: packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_LOADTRUE:   packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_LOADNIL:    packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_GETUPVAL:   packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_SETUPVAL:   packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_GETTABUP:   packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_GETTABLE:   packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_GETI:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_GETFIELD:   packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_SETTABUP:   packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_SETTABLE:   packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_SETI:       packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_SETFIELD:   packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_NEWTABLE:   packOpMode(0, 0, 0, 0, 1, modevABC),
	bytecode.OP_SELF:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_ADDI:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_ADDK:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_SUBK:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_MULK:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_MODK:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_POWK:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_DIVK:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_IDIVK:      packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_BANDK:      packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_BORK:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_BXORK:      packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_SHLI:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_SHRI:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_ADD:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_SUB:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_MUL:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_MOD:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_POW:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_DIV:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_IDIV:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_BAND:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_BOR:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_BXOR:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_SHL:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_SHR:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_MMBIN:      packOpMode(1, 0, 0, 0, 0, modeABC),
	bytecode.OP_MMBINI:     packOpMode(1, 0, 0, 0, 0, modeABC),
	bytecode.OP_MMBINK:     packOpMode(1, 0, 0, 0, 0, modeABC),
	bytecode.OP_UNM:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_BNOT:       packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_NOT:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_LEN:        packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_CONCAT:     packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_CLOSE:      packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_TBC:        packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_JMP:        packOpMode(0, 0, 0, 0, 0, modeSJ),
	bytecode.OP_EQ:         packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_LT:         packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_LE:         packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_EQK:        packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_EQI:        packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_LTI:        packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_LEI:        packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_GTI:        packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_GEI:        packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_TEST:       packOpMode(0, 0, 0, 1, 0, modeABC),
	bytecode.OP_TESTSET:    packOpMode(0, 0, 0, 1, 1, modeABC),
	bytecode.OP_CALL:       packOpMode(0, 1, 1, 0, 1, modeABC),
	bytecode.OP_TAILCALL:   packOpMode(0, 1, 1, 0, 1, modeABC),
	bytecode.OP_RETURN:     packOpMode(0, 0, 1, 0, 0, modeABC),
	bytecode.OP_RETURN0:    packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_RETURN1:    packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_FORLOOP:    packOpMode(0, 0, 0, 0, 1, modeABx),
	bytecode.OP_FORPREP:    packOpMode(0, 0, 0, 0, 1, modeABx),
	bytecode.OP_TFORPREP:   packOpMode(0, 0, 0, 0, 0, modeABx),
	bytecode.OP_TFORCALL:   packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_TFORLOOP:   packOpMode(0, 0, 0, 0, 1, modeABx),
	bytecode.OP_SETLIST:    packOpMode(0, 0, 1, 0, 0, modevABC),
	bytecode.OP_CLOSURE:    packOpMode(0, 0, 0, 0, 1, modeABx),
	bytecode.OP_VARARG:     packOpMode(0, 1, 0, 0, 1, modeABC),
	bytecode.OP_GETVARG:    packOpMode(0, 0, 0, 0, 1, modeABC),
	bytecode.OP_ERRNNIL:    packOpMode(0, 0, 0, 0, 0, modeABx),
	bytecode.OP_VARARGPREP: packOpMode(0, 0, 0, 0, 0, modeABC),
	bytecode.OP_EXTRAARG:   packOpMode(0, 0, 0, 0, 0, modeAx),
}

func testTMode(op bytecode.OpCode) bool {
	return (opModes[op] & (1 << 4)) != 0
}

func mask1(n, p uint) bytecode.Instruction {
	return ((^bytecode.Instruction(0)) >> (32 - n)) << p
}

func mask0(n, p uint) bytecode.Instruction {
	return ^mask1(n, p)
}

func setOp(i *bytecode.Instruction, o bytecode.OpCode) {
	*i = (*i & mask0(bytecode.SizeOp, bytecode.PosOp)) |
		((bytecode.Instruction(o) << bytecode.PosOp) & mask1(bytecode.SizeOp, bytecode.PosOp))
}

func setArgA(i *bytecode.Instruction, v int) {
	*i = (*i & mask0(bytecode.SizeA, bytecode.PosA)) |
		((bytecode.Instruction(uint32(v)) << bytecode.PosA) & mask1(bytecode.SizeA, bytecode.PosA))
}

func setArgB(i *bytecode.Instruction, v int) {
	*i = (*i & mask0(bytecode.SizeB, bytecode.PosB)) |
		((bytecode.Instruction(uint32(v)) << bytecode.PosB) & mask1(bytecode.SizeB, bytecode.PosB))
}

func setArgC(i *bytecode.Instruction, v int) {
	*i = (*i & mask0(bytecode.SizeC, bytecode.PosC)) |
		((bytecode.Instruction(uint32(v)) << bytecode.PosC) & mask1(bytecode.SizeC, bytecode.PosC))
}

func setArgK(i *bytecode.Instruction, v int) {
	*i = (*i & mask0(bytecode.SizeK, bytecode.PosK)) |
		((bytecode.Instruction(uint32(v)) << bytecode.PosK) & mask1(bytecode.SizeK, bytecode.PosK))
}

func setArgBx(i *bytecode.Instruction, v int) {
	*i = (*i & mask0(bytecode.SizeBx, bytecode.PosBx)) |
		((bytecode.Instruction(uint32(v)) << bytecode.PosBx) & mask1(bytecode.SizeBx, bytecode.PosBx))
}

func setArgSJ(i *bytecode.Instruction, sj int) {
	j := uint32(sj + bytecode.OffsetSJ)
	*i = (*i & mask0(bytecode.SizesJ, bytecode.PossJ)) |
		((bytecode.Instruction(j) << bytecode.PossJ) & mask1(bytecode.SizesJ, bytecode.PossJ))
}

func createABCk(o bytecode.OpCode, a, b, c, k int) bytecode.Instruction {
	return (bytecode.Instruction(o) << bytecode.PosOp) |
		(bytecode.Instruction(uint32(a)&bytecode.MaxArgA) << bytecode.PosA) |
		(bytecode.Instruction(uint32(b)&bytecode.MaxArgB) << bytecode.PosB) |
		(bytecode.Instruction(uint32(c)&bytecode.MaxArgC) << bytecode.PosC) |
		(bytecode.Instruction(uint32(k)&1) << bytecode.PosK)
}

func createvABCk(o bytecode.OpCode, a, vb, vc, k int) bytecode.Instruction {
	return (bytecode.Instruction(o) << bytecode.PosOp) |
		(bytecode.Instruction(uint32(a)&bytecode.MaxArgA) << bytecode.PosA) |
		(bytecode.Instruction(uint32(vb)&bytecode.MaxArgvB) << bytecode.PosvB) |
		(bytecode.Instruction(uint32(vc)&bytecode.MaxArgvC) << bytecode.PosvC) |
		(bytecode.Instruction(uint32(k)&1) << bytecode.PosK)
}

func createABx(o bytecode.OpCode, a, bx int) bytecode.Instruction {
	return (bytecode.Instruction(o) << bytecode.PosOp) |
		(bytecode.Instruction(uint32(a)&bytecode.MaxArgA) << bytecode.PosA) |
		(bytecode.Instruction(uint32(bx)&bytecode.MaxArgBx) << bytecode.PosBx)
}

func createAx(o bytecode.OpCode, ax int) bytecode.Instruction {
	return (bytecode.Instruction(o) << bytecode.PosOp) |
		(bytecode.Instruction(uint32(ax)&bytecode.MaxArgAx) << bytecode.PosAx)
}

func createSJ(o bytecode.OpCode, j int) bytecode.Instruction {
	return (bytecode.Instruction(o) << bytecode.PosOp) |
		(bytecode.Instruction(uint32(j)&bytecode.MaxArgsJ) << bytecode.PossJ)
}

func int2sC(i int) int { return i + bytecode.OffsetSC }

func fitsC(i int64) bool {
	return uint64(i)+uint64(bytecode.OffsetSC) <= uint64(bytecode.MaxArgC)
}

func fitsBx(i int64) bool {
	return -int64(bytecode.OffsetSBx) <= i && i <= int64(bytecode.MaxArgBx-bytecode.OffsetSBx)
}

// Numeric helpers for constant folding matching lvm.c and lobject.c.
func fltToIntEq(n float64) (int64, bool) {
	f := math.Floor(n)
	if n != f {
		return 0, false
	}
	const minIntF = float64(math.MinInt64) // exact -2^63
	if f >= minIntF && f < -minIntF {
		return int64(f), true
	}
	return 0, false
}

func constToIntNS(c bytecode.Constant) (int64, bool) {
	switch c.Kind {
	case bytecode.ConstInt:
		return c.Int, true
	case bytecode.ConstFloat:
		return fltToIntEq(c.Float)
	default:
		return 0, false
	}
}

func constToNumNS(c bytecode.Constant) (float64, bool) {
	switch c.Kind {
	case bytecode.ConstFloat:
		return c.Float, true
	case bytecode.ConstInt:
		return float64(c.Int), true
	default:
		return 0, false
	}
}

func shiftLeft(x, y int64) int64 {
	if y < 0 {
		if y <= -64 {
			return 0
		}
		return int64(uint64(x) >> uint64(-y))
	}
	if y >= 64 {
		return 0
	}
	return int64(uint64(x) << uint64(y))
}

func intIDiv(m, n int64) int64 {
	if n == -1 {
		return int64(0 - uint64(m))
	}
	q := m / n
	if (m^n) < 0 && m%n != 0 {
		q--
	}
	return q
}

func intMod(m, n int64) int64 {
	if n == -1 {
		return 0
	}
	r := m % n
	if r != 0 && (r^n) < 0 {
		r += n
	}
	return r
}

func intArith(op int, v1, v2 int64) int64 {
	switch op {
	case luaOpAdd:
		return int64(uint64(v1) + uint64(v2))
	case luaOpSub:
		return int64(uint64(v1) - uint64(v2))
	case luaOpMul:
		return int64(uint64(v1) * uint64(v2))
	case luaOpMod:
		return intMod(v1, v2)
	case luaOpIDiv:
		return intIDiv(v1, v2)
	case luaOpBAnd:
		return v1 & v2
	case luaOpBOr:
		return v1 | v2
	case luaOpBXor:
		return v1 ^ v2
	case luaOpShl:
		return shiftLeft(v1, v2)
	case luaOpShr:
		return shiftLeft(v1, int64(0-uint64(v2)))
	case luaOpUnm:
		return int64(0 - uint64(v1))
	case luaOpBNot:
		return ^v1
	default:
		panic("unreachable intArith op")
	}
}

func numArith(op int, v1, v2 float64) float64 {
	switch op {
	case luaOpAdd:
		return v1 + v2
	case luaOpSub:
		return v1 - v2
	case luaOpMul:
		return v1 * v2
	case luaOpDiv:
		return v1 / v2
	case luaOpPow:
		if v2 == 2 {
			return v1 * v1
		}
		return math.Pow(v1, v2)
	case luaOpIDiv:
		return math.Floor(v1 / v2)
	case luaOpUnm:
		return -v1
	case luaOpMod:
		m := math.Mod(v1, v2)
		if (m > 0 && v2 < 0) || (m < 0 && v2 > 0) {
			m += v2
		}
		return m
	default:
		panic("unreachable numArith op")
	}
}

func rawArith(op int, p1, p2 bytecode.Constant) (bytecode.Constant, bool) {
	switch op {
	case luaOpBAnd, luaOpBOr, luaOpBXor, luaOpShl, luaOpShr, luaOpBNot:
		i1, ok1 := constToIntNS(p1)
		i2, ok2 := constToIntNS(p2)
		if ok1 && ok2 {
			return bytecode.Constant{Kind: bytecode.ConstInt, Int: intArith(op, i1, i2)}, true
		}
		return bytecode.Constant{}, false
	case luaOpDiv, luaOpPow:
		n1, ok1 := constToNumNS(p1)
		n2, ok2 := constToNumNS(p2)
		if ok1 && ok2 {
			return bytecode.Constant{Kind: bytecode.ConstFloat, Float: numArith(op, n1, n2)}, true
		}
		return bytecode.Constant{}, false
	default:
		if p1.Kind == bytecode.ConstInt && p2.Kind == bytecode.ConstInt {
			return bytecode.Constant{Kind: bytecode.ConstInt, Int: intArith(op, p1.Int, p2.Int)}, true
		}
		n1, ok1 := constToNumNS(p1)
		n2, ok2 := constToNumNS(p2)
		if ok1 && ok2 {
			return bytecode.Constant{Kind: bytecode.ConstFloat, Float: numArith(op, n1, n2)}, true
		}
		return bytecode.Constant{}, false
	}
}

func validFoldOp(op int, v1, v2 bytecode.Constant) bool {
	switch op {
	case luaOpBAnd, luaOpBOr, luaOpBXor, luaOpShl, luaOpShr, luaOpBNot:
		_, ok1 := constToIntNS(v1)
		_, ok2 := constToIntNS(v2)
		return ok1 && ok2
	case luaOpDiv, luaOpIDiv, luaOpMod:
		n2, ok := constToNumNS(v2)
		return ok && n2 != 0
	default:
		return true
	}
}

func toNumeral(e *expDesc) (bytecode.Constant, bool) {
	if e.hasJumps() {
		return bytecode.Constant{}, false
	}
	switch e.k {
	case vKInt:
		return bytecode.Constant{Kind: bytecode.ConstInt, Int: e.ival}, true
	case vKFlt:
		return bytecode.Constant{Kind: bytecode.ConstFloat, Float: e.nval}, true
	default:
		return bytecode.Constant{}, false
	}
}

func const2Exp(c bytecode.Constant, e *expDesc) {
	switch c.Kind {
	case bytecode.ConstInt:
		e.k = vKInt
		e.ival = c.Int
	case bytecode.ConstFloat:
		e.k = vKFlt
		e.nval = c.Float
	case bytecode.ConstFalse:
		e.k = vFalse
	case bytecode.ConstTrue:
		e.k = vTrue
	case bytecode.ConstNil:
		e.k = vNil
	case bytecode.ConstString:
		e.k = vKStr
		e.strval = c.Str
	default:
		panic("unexpected constant kind")
	}
}
