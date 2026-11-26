package bytecode

// Instruction is a 32-bit PUC-Rio Lua 5.5.1 bytecode instruction.
type Instruction uint32

// OpCode matches PUC-Rio Lua 5.5.1 lopcodes.h byte-for-byte (85 opcodes: 0..84).
type OpCode uint8

const (
	OP_MOVE OpCode = iota
	OP_LOADI
	OP_LOADF
	OP_LOADK
	OP_LOADKX
	OP_LOADFALSE
	OP_LFALSESKIP
	OP_LOADTRUE
	OP_LOADNIL
	OP_GETUPVAL
	OP_SETUPVAL

	OP_GETTABUP
	OP_GETTABLE
	OP_GETI
	OP_GETFIELD

	OP_SETTABUP
	OP_SETTABLE
	OP_SETI
	OP_SETFIELD

	OP_NEWTABLE

	OP_SELF

	OP_ADDI

	OP_ADDK
	OP_SUBK
	OP_MULK
	OP_MODK
	OP_POWK
	OP_DIVK
	OP_IDIVK

	OP_BANDK
	OP_BORK
	OP_BXORK

	OP_SHLI
	OP_SHRI

	OP_ADD
	OP_SUB
	OP_MUL
	OP_MOD
	OP_POW
	OP_DIV
	OP_IDIV

	OP_BAND
	OP_BOR
	OP_BXOR
	OP_SHL
	OP_SHR

	OP_MMBIN
	OP_MMBINI
	OP_MMBINK

	OP_UNM
	OP_BNOT
	OP_NOT
	OP_LEN

	OP_CONCAT

	OP_CLOSE
	OP_TBC
	OP_JMP
	OP_EQ
	OP_LT
	OP_LE

	OP_EQK
	OP_EQI
	OP_LTI
	OP_LEI
	OP_GTI
	OP_GEI

	OP_TEST
	OP_TESTSET

	OP_CALL
	OP_TAILCALL

	OP_RETURN
	OP_RETURN0
	OP_RETURN1

	OP_FORLOOP
	OP_FORPREP

	OP_TFORPREP
	OP_TFORCALL
	OP_TFORLOOP

	OP_SETLIST

	OP_CLOSURE

	OP_VARARG

	OP_GETVARG

	OP_ERRNNIL

	OP_VARARGPREP

	OP_EXTRAARG

	NumOpcodes
)

// Bitfield constants matching PUC-Rio Lua 5.5.1 lopcodes.h.
const (
	SizeOp = 7
	SizeA  = 8
	SizeK  = 1
	SizeB  = 8
	SizevB = 6
	SizeC  = 8
	SizevC = 10
	SizeBx = SizeC + SizeB + SizeK // 17
	SizeAx = SizeBx + SizeA        // 25
	SizesJ = SizeBx + SizeA        // 25

	PosOp = 0
	PosA  = PosOp + SizeOp // 7
	PosK  = PosA + SizeA   // 15
	PosB  = PosK + SizeK   // 16
	PosvB = PosK + SizeK   // 16
	PosC  = PosB + SizeB   // 24
	PosvC = PosvB + SizevB // 22
	PosBx = PosK           // 15
	PosAx = PosA           // 7
	PossJ = PosA           // 7

	MaxArgA  = (1 << SizeA) - 1  // 255
	MaxArgB  = (1 << SizeB) - 1  // 255
	MaxArgvB = (1 << SizevB) - 1 // 63
	MaxArgC  = (1 << SizeC) - 1  // 255
	MaxArgvC = (1 << SizevC) - 1 // 1023
	MaxArgBx = (1 << SizeBx) - 1 // 131071
	MaxArgAx = (1 << SizeAx) - 1 // 33554431
	MaxArgsJ = (1 << SizesJ) - 1 // 33554431

	OffsetSC  = MaxArgC >> 1  // 127
	OffsetSBx = MaxArgBx >> 1 // 65535
	OffsetSJ  = MaxArgsJ >> 1 // 16777215
)

func (i Instruction) Op() OpCode { return OpCode(i & 0x7f) }
func (i Instruction) A() int     { return int((i >> PosA) & 0xff) }
func (i Instruction) A8() uint8  { return uint8(i >> PosA) }
func (i Instruction) B() int     { return int((i >> PosB) & 0xff) }
func (i Instruction) B8() uint8  { return uint8(i >> PosB) }
func (i Instruction) SB() int    { return int((i>>PosB)&0xff) - OffsetSC }
func (i Instruction) VB() int    { return int((i >> PosvB) & 0x3f) }
func (i Instruction) C() int     { return int((i >> PosC) & 0xff) }
func (i Instruction) C8() uint8  { return uint8(i >> PosC) }
func (i Instruction) SC() int    { return int((i>>PosC)&0xff) - OffsetSC }
func (i Instruction) VC() int    { return int((i >> PosvC) & 0x3ff) }
func (i Instruction) K() bool    { return (i & (1 << PosK)) != 0 }
func (i Instruction) KBit() int  { return int((i >> PosK) & 1) }
func (i Instruction) Bx() int    { return int((i >> PosBx) & MaxArgBx) }
func (i Instruction) SBx() int   { return int((i>>PosBx)&MaxArgBx) - OffsetSBx }
func (i Instruction) Ax() int    { return int((i >> PosAx) & MaxArgAx) }
func (i Instruction) SJ() int    { return int((i>>PossJ)&MaxArgsJ) - OffsetSJ }

// Proto flags matching lobject.h.
const (
	PFVarargHidden uint8 = 1 // PF_VAHID
	PFVarargTable  uint8 = 2 // PF_VATAB
	PFFixed        uint8 = 4 // PF_FIXED
)

// ConstKind discriminates Proto constants.
type ConstKind uint8

const (
	ConstNil ConstKind = iota
	ConstFalse
	ConstTrue
	ConstInt
	ConstFloat
	ConstString
)

// Constant is an immutable constant in a Proto.
type Constant struct {
	Kind    ConstKind
	Int     int64
	Float   float64
	Str     string
	IsShort bool
}

// UpvalDesc describes an upvalue capture for a Proto.
type UpvalDesc struct {
	Name    string
	InStack uint8
	Idx     uint8
	Kind    uint8
}

// LocVar holds debug metadata for a local variable.
type LocVar struct {
	Name    string
	StartPC int
	EndPC   int
}

// AbsLineInfo holds an absolute line checkpoint.
type AbsLineInfo struct {
	PC   int32
	Line int32
}

// Proto is an immutable, goroutine-shareable Lua 5.5.1 function prototype.
type Proto struct {
	Source          string
	NoSource        bool // true when binary chunk had NULL source (stripped dump -> "=?" / "?")
	LineDefined     int
	LastLineDefined int
	NumParams       uint8
	Flag            uint8
	MaxStackSize    uint8

	Code        []Instruction
	Consts      []Constant
	Upvalues    []UpvalDesc
	Protos      []*Proto
	LineInfo    []int8
	AbsLineInfo []AbsLineInfo
	LocVars     []LocVar

	// IC metadata computed during verification (immutable, shared across States).
	ICCount int
	// ICSlot maps a PC to its IC slot index, or -1 if that PC has no IC.
	ICSlot []int32
}

func (p *Proto) IsVararg() bool {
	return p.Flag&(PFVarargHidden|PFVarargTable) != 0
}

// OpNames lists opcode names for diagnostics and listings.
var OpNames = [NumOpcodes]string{
	OP_MOVE:       "MOVE",
	OP_LOADI:      "LOADI",
	OP_LOADF:      "LOADF",
	OP_LOADK:      "LOADK",
	OP_LOADKX:     "LOADKX",
	OP_LOADFALSE:  "LOADFALSE",
	OP_LFALSESKIP: "LFALSESKIP",
	OP_LOADTRUE:   "LOADTRUE",
	OP_LOADNIL:    "LOADNIL",
	OP_GETUPVAL:   "GETUPVAL",
	OP_SETUPVAL:   "SETUPVAL",
	OP_GETTABUP:   "GETTABUP",
	OP_GETTABLE:   "GETTABLE",
	OP_GETI:       "GETI",
	OP_GETFIELD:   "GETFIELD",
	OP_SETTABUP:   "SETTABUP",
	OP_SETTABLE:   "SETTABLE",
	OP_SETI:       "SETI",
	OP_SETFIELD:   "SETFIELD",
	OP_NEWTABLE:   "NEWTABLE",
	OP_SELF:       "SELF",
	OP_ADDI:       "ADDI",
	OP_ADDK:       "ADDK",
	OP_SUBK:       "SUBK",
	OP_MULK:       "MULK",
	OP_MODK:       "MODK",
	OP_POWK:       "POWK",
	OP_DIVK:       "DIVK",
	OP_IDIVK:      "IDIVK",
	OP_BANDK:      "BANDK",
	OP_BORK:       "BORK",
	OP_BXORK:      "BXORK",
	OP_SHLI:       "SHLI",
	OP_SHRI:       "SHRI",
	OP_ADD:        "ADD",
	OP_SUB:        "SUB",
	OP_MUL:        "MUL",
	OP_MOD:        "MOD",
	OP_POW:        "POW",
	OP_DIV:        "DIV",
	OP_IDIV:       "IDIV",
	OP_BAND:       "BAND",
	OP_BOR:        "BOR",
	OP_BXOR:       "BXOR",
	OP_SHL:        "SHL",
	OP_SHR:        "SHR",
	OP_MMBIN:      "MMBIN",
	OP_MMBINI:     "MMBINI",
	OP_MMBINK:     "MMBINK",
	OP_UNM:        "UNM",
	OP_BNOT:       "BNOT",
	OP_NOT:        "NOT",
	OP_LEN:        "LEN",
	OP_CONCAT:     "CONCAT",
	OP_CLOSE:      "CLOSE",
	OP_TBC:        "TBC",
	OP_JMP:        "JMP",
	OP_EQ:         "EQ",
	OP_LT:         "LT",
	OP_LE:         "LE",
	OP_EQK:        "EQK",
	OP_EQI:        "EQI",
	OP_LTI:        "LTI",
	OP_LEI:        "LEI",
	OP_GTI:        "GTI",
	OP_GEI:        "GEI",
	OP_TEST:       "TEST",
	OP_TESTSET:    "TESTSET",
	OP_CALL:       "CALL",
	OP_TAILCALL:   "TAILCALL",
	OP_RETURN:     "RETURN",
	OP_RETURN0:    "RETURN0",
	OP_RETURN1:    "RETURN1",
	OP_FORLOOP:    "FORLOOP",
	OP_FORPREP:    "FORPREP",
	OP_TFORPREP:   "TFORPREP",
	OP_TFORCALL:   "TFORCALL",
	OP_TFORLOOP:   "TFORLOOP",
	OP_SETLIST:    "SETLIST",
	OP_CLOSURE:    "CLOSURE",
	OP_VARARG:     "VARARG",
	OP_GETVARG:    "GETVARG",
	OP_ERRNNIL:    "ERRNNIL",
	OP_VARARGPREP: "VARARGPREP",
	OP_EXTRAARG:   "EXTRAARG",
}
