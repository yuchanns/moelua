//go:build (arm64 || amd64) && !purego && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

const asmDispatchEnabled = true

// asmPollInterval bounds how many backward edges or fast Lua-to-Lua calls execute inside
// the NOSPLIT Plan 9 interpreter loop before yielding briefly to Go for GC/preemption safepoints.
const asmPollInterval = 8192

const (
	asmExitDone   uint64 = 0
	asmExitPoll   uint64 = 1
	asmExitReload uint64 = 2
	asmExitCold   uint64 = 3
)

// asmExecCtx is a pointer-free execution context passed on the Go stack to the Plan 9
// direct-threaded bytecode interpreter. Every backing buffer referenced by uintptr here
// is strongly rooted by State, Thread, and boundProto in (*State).runAsm and never moves
// while the leaf assembly loop runs (all allocations exit via asmExitCold).
type asmExecCtx struct {
	regs       uintptr // +0:   &stack[base] (frame R[0])
	insns      uintptr // +8:   &bp.insns[0]
	consts     uintptr // +16:  &bp.consts[0]
	pagePtrs   uintptr // +24:  &s.arena.pagePtrs[0]
	largeSpans uintptr // +32:  &s.arena.largeSpans[0] (24B slice headers)
	stack      uintptr // +40:  &th.stack[0]
	frames     uintptr // +48:  &th.frames[0]
	protos     uintptr // +56:  &s.protos[0]
	ic         uintptr // +64:  &bp.ic[0]
	icSlots    uintptr // +72:  &bp.icSlots[0]
	clUpvals   uintptr // +80:  &cl.upvals[0] (0 if nUpvals == 0)
	curBP      uintptr // +88:  uintptr(unsafe.Pointer(ev.bp))
	th         uintptr // +96:  uintptr(unsafe.Pointer(th))
	protoEpoch uint64  // +104: s.protoEpoch
	pc         int64   // +112: current instruction index
	poll       int64   // +120: countdown budget before asmExitPoll
	ci         int64   // +128: active frame index (th.nframe - 1)
	base       int64   // +136: active frame register base
	entryDepth int64   // +144: return when th.nframe <= entryDepth
	thSelf     uint32  // +152: th.self (arena.ObjRef)
	sqrtIdx    uint32  // +156: staticNative idx for math.sqrt (0 if unregistered)
	absIdx     uint32  // +160: staticNative idx for math.abs (0 if unregistered)
	_          uint32  // +164: pad -> 168B exact
}

// Compile-time layout assertions matching vm/asm_offsets.h byte-for-byte.
const (
	_ = uint(168 - unsafe.Sizeof(asmExecCtx{}))
	_ = uint(unsafe.Sizeof(asmExecCtx{}) - 168)

	_ = uint(unsafe.Offsetof(asmExecCtx{}.regs) - 0)
	_ = uint(0 - unsafe.Offsetof(asmExecCtx{}.regs))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.insns) - 8)
	_ = uint(8 - unsafe.Offsetof(asmExecCtx{}.insns))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.consts) - 16)
	_ = uint(16 - unsafe.Offsetof(asmExecCtx{}.consts))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.pagePtrs) - 24)
	_ = uint(24 - unsafe.Offsetof(asmExecCtx{}.pagePtrs))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.largeSpans) - 32)
	_ = uint(32 - unsafe.Offsetof(asmExecCtx{}.largeSpans))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.stack) - 40)
	_ = uint(40 - unsafe.Offsetof(asmExecCtx{}.stack))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.frames) - 48)
	_ = uint(48 - unsafe.Offsetof(asmExecCtx{}.frames))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.protos) - 56)
	_ = uint(56 - unsafe.Offsetof(asmExecCtx{}.protos))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.ic) - 64)
	_ = uint(64 - unsafe.Offsetof(asmExecCtx{}.ic))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.icSlots) - 72)
	_ = uint(72 - unsafe.Offsetof(asmExecCtx{}.icSlots))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.clUpvals) - 80)
	_ = uint(80 - unsafe.Offsetof(asmExecCtx{}.clUpvals))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.curBP) - 88)
	_ = uint(88 - unsafe.Offsetof(asmExecCtx{}.curBP))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.th) - 96)
	_ = uint(96 - unsafe.Offsetof(asmExecCtx{}.th))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.protoEpoch) - 104)
	_ = uint(104 - unsafe.Offsetof(asmExecCtx{}.protoEpoch))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.pc) - 112)
	_ = uint(112 - unsafe.Offsetof(asmExecCtx{}.pc))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.poll) - 120)
	_ = uint(120 - unsafe.Offsetof(asmExecCtx{}.poll))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.ci) - 128)
	_ = uint(128 - unsafe.Offsetof(asmExecCtx{}.ci))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.base) - 136)
	_ = uint(136 - unsafe.Offsetof(asmExecCtx{}.base))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.entryDepth) - 144)
	_ = uint(144 - unsafe.Offsetof(asmExecCtx{}.entryDepth))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.thSelf) - 152)
	_ = uint(152 - unsafe.Offsetof(asmExecCtx{}.thSelf))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.sqrtIdx) - 156)
	_ = uint(156 - unsafe.Offsetof(asmExecCtx{}.sqrtIdx))
	_ = uint(unsafe.Offsetof(asmExecCtx{}.absIdx) - 160)
	_ = uint(160 - unsafe.Offsetof(asmExecCtx{}.absIdx))

	// Value
	_ = uint(unsafe.Offsetof(Value{}.bits) - 0)
	_ = uint(unsafe.Offsetof(Value{}.tag) - 8)
	_ = uint(tagInt - 0)
	_ = uint(tagFloat - 1)

	// Table
	_ = uint(unsafe.Offsetof(Table{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Mark) - 5)
	_ = uint(5 - (unsafe.Offsetof(Table{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Mark)))
	_ = uint(unsafe.Offsetof(Table{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Flags) - 7)
	_ = uint(7 - (unsafe.Offsetof(Table{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Flags)))
	_ = uint(unsafe.Offsetof(Table{}.self) - 8)
	_ = uint(8 - unsafe.Offsetof(Table{}.self))
	_ = uint(unsafe.Offsetof(Table{}.shapeRef) - 12)
	_ = uint(12 - unsafe.Offsetof(Table{}.shapeRef))
	_ = uint(unsafe.Offsetof(Table{}.metaRef) - 16)
	_ = uint(16 - unsafe.Offsetof(Table{}.metaRef))
	_ = uint(unsafe.Offsetof(Table{}.hashRef) - 20)
	_ = uint(20 - unsafe.Offsetof(Table{}.hashRef))
	_ = uint(unsafe.Offsetof(Table{}.arrRef) - 24)
	_ = uint(24 - unsafe.Offsetof(Table{}.arrRef))
	_ = uint(unsafe.Offsetof(Table{}.slotsRef) - 28)
	_ = uint(28 - unsafe.Offsetof(Table{}.slotsRef))
	_ = uint(unsafe.Offsetof(Table{}.arrLen) - 32)
	_ = uint(32 - unsafe.Offsetof(Table{}.arrLen))
	_ = uint(unsafe.Offsetof(Table{}.arrCap) - 36)
	_ = uint(36 - unsafe.Offsetof(Table{}.arrCap))
	_ = uint(unsafe.Offsetof(Table{}.slotsLen) - 44)
	_ = uint(44 - unsafe.Offsetof(Table{}.slotsLen))
	_ = uint(unsafe.Offsetof(Table{}.slotsCap) - 45)
	_ = uint(45 - unsafe.Offsetof(Table{}.slotsCap))
	_ = uint(unsafe.Offsetof(Table{}.arrKind) - 46)
	_ = uint(46 - unsafe.Offsetof(Table{}.arrKind))
	_ = uint(unsafe.Offsetof(Table{}.tmFlags) - 47)
	_ = uint(47 - unsafe.Offsetof(Table{}.tmFlags))
	_ = uint(unsafe.Offsetof(Table{}.inlineSlots) - 48)
	_ = uint(48 - unsafe.Offsetof(Table{}.inlineSlots))

	// Closure
	_ = uint(unsafe.Offsetof(Closure{}.self) - 8)
	_ = uint(8 - unsafe.Offsetof(Closure{}.self))
	_ = uint(unsafe.Offsetof(Closure{}.upvalsRef) - 16)
	_ = uint(16 - unsafe.Offsetof(Closure{}.upvalsRef))
	_ = uint(unsafe.Offsetof(Closure{}.protoIdx) - 20)
	_ = uint(20 - unsafe.Offsetof(Closure{}.protoIdx))

	// Upvalue
	_ = uint(unsafe.Offsetof(Upvalue{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Mark) - 5)
	_ = uint(5 - (unsafe.Offsetof(Upvalue{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Mark)))
	_ = uint(unsafe.Offsetof(Upvalue{}.ownerRef) - 12)
	_ = uint(12 - unsafe.Offsetof(Upvalue{}.ownerRef))
	_ = uint(unsafe.Offsetof(Upvalue{}.closed) - 16)
	_ = uint(16 - unsafe.Offsetof(Upvalue{}.closed))
	_ = uint(unsafe.Offsetof(Upvalue{}.stackIdx) - 40)
	_ = uint(40 - unsafe.Offsetof(Upvalue{}.stackIdx))

	// ICEntry & polyEntry
	_ = uint(unsafe.Offsetof(ICEntry{}.val) - 0)
	_ = uint(unsafe.Offsetof(ICEntry{}.epoch) - 16)
	_ = uint(16 - unsafe.Offsetof(ICEntry{}.epoch))
	_ = uint(unsafe.Offsetof(ICEntry{}.shapeRef) - 24)
	_ = uint(24 - unsafe.Offsetof(ICEntry{}.shapeRef))
	_ = uint(unsafe.Offsetof(ICEntry{}.targetShapeRef) - 28)
	_ = uint(28 - unsafe.Offsetof(ICEntry{}.targetShapeRef))
	_ = uint(unsafe.Offsetof(ICEntry{}.holderRef) - 32)
	_ = uint(32 - unsafe.Offsetof(ICEntry{}.holderRef))
	_ = uint(unsafe.Offsetof(ICEntry{}.polyRef) - 36)
	_ = uint(36 - unsafe.Offsetof(ICEntry{}.polyRef))
	_ = uint(unsafe.Offsetof(ICEntry{}.slot) - 40)
	_ = uint(40 - unsafe.Offsetof(ICEntry{}.slot))
	_ = uint(unsafe.Offsetof(ICEntry{}.kind) - 44)
	_ = uint(44 - unsafe.Offsetof(ICEntry{}.kind))
	_ = uint(unsafe.Offsetof(ICEntry{}.noMeta) - 45)
	_ = uint(45 - unsafe.Offsetof(ICEntry{}.noMeta))
	_ = uint(unsafe.Offsetof(ICEntry{}.protoAbsent) - 46)
	_ = uint(46 - unsafe.Offsetof(ICEntry{}.protoAbsent))
	_ = uint(unsafe.Offsetof(ICEntry{}.polyCount) - 47)
	_ = uint(47 - unsafe.Offsetof(ICEntry{}.polyCount))

	_ = uint(unsafe.Offsetof(polyEntry{}.val) - 0)
	_ = uint(unsafe.Offsetof(polyEntry{}.epoch) - 16)
	_ = uint(16 - unsafe.Offsetof(polyEntry{}.epoch))
	_ = uint(unsafe.Offsetof(polyEntry{}.shapeRef) - 24)
	_ = uint(24 - unsafe.Offsetof(polyEntry{}.shapeRef))
	_ = uint(unsafe.Offsetof(polyEntry{}.targetShapeRef) - 28)
	_ = uint(28 - unsafe.Offsetof(polyEntry{}.targetShapeRef))
	_ = uint(unsafe.Offsetof(polyEntry{}.slot) - 32)
	_ = uint(32 - unsafe.Offsetof(polyEntry{}.slot))
	_ = uint(unsafe.Offsetof(polyEntry{}.kind) - 36)
	_ = uint(36 - unsafe.Offsetof(polyEntry{}.kind))
	_ = uint(unsafe.Offsetof(polyEntry{}.noMeta) - 37)
	_ = uint(37 - unsafe.Offsetof(polyEntry{}.noMeta))
	_ = uint(unsafe.Offsetof(polyEntry{}.protoAbsent) - 38)
	_ = uint(38 - unsafe.Offsetof(polyEntry{}.protoAbsent))

	// tableHash & hashNode
	_ = uint(unsafe.Offsetof(tableHash{}.nodesRef) - 0)
	_ = uint(unsafe.Offsetof(tableHash{}.dictGen) - 8)
	_ = uint(8 - unsafe.Offsetof(tableHash{}.dictGen))
	_ = uint(unsafe.Offsetof(hashNode{}.val) - 16)
	_ = uint(16 - unsafe.Offsetof(hashNode{}.val))

	// boundProto
	_ = uint(unsafe.Offsetof(boundProto{}.insns) - 8)
	_ = uint(8 - unsafe.Offsetof(boundProto{}.insns))
	_ = uint(unsafe.Offsetof(boundProto{}.consts) - 16)
	_ = uint(16 - unsafe.Offsetof(boundProto{}.consts))
	_ = uint(unsafe.Offsetof(boundProto{}.icSlots) - 40)
	_ = uint(40 - unsafe.Offsetof(boundProto{}.icSlots))
	_ = uint(unsafe.Offsetof(boundProto{}.ic) - 64)
	_ = uint(64 - unsafe.Offsetof(boundProto{}.ic))
	_ = uint(unsafe.Offsetof(boundProto{}.numParams) - 128)
	_ = uint(128 - unsafe.Offsetof(boundProto{}.numParams))
	_ = uint(unsafe.Offsetof(boundProto{}.maxStack) - 129)
	_ = uint(129 - unsafe.Offsetof(boundProto{}.maxStack))
	_ = uint(unsafe.Offsetof(boundProto{}.isVararg) - 130)
	_ = uint(130 - unsafe.Offsetof(boundProto{}.isVararg))

	// callFrame
	_ = uint(200 - unsafe.Sizeof(callFrame{}))
	_ = uint(unsafe.Sizeof(callFrame{}) - 200)
	_ = uint(unsafe.Offsetof(callFrame{}.kind) - 0)
	_ = uint(unsafe.Offsetof(callFrame{}.clRef) - 4)
	_ = uint(4 - unsafe.Offsetof(callFrame{}.clRef))
	_ = uint(unsafe.Offsetof(callFrame{}.varargTabRef) - 8)
	_ = uint(8 - unsafe.Offsetof(callFrame{}.varargTabRef))
	_ = uint(unsafe.Offsetof(callFrame{}.base) - 16)
	_ = uint(16 - unsafe.Offsetof(callFrame{}.base))
	_ = uint(unsafe.Offsetof(callFrame{}.funcIdx) - 24)
	_ = uint(24 - unsafe.Offsetof(callFrame{}.funcIdx))
	_ = uint(unsafe.Offsetof(callFrame{}.pc) - 32)
	_ = uint(32 - unsafe.Offsetof(callFrame{}.pc))
	_ = uint(unsafe.Offsetof(callFrame{}.nresults) - 40)
	_ = uint(40 - unsafe.Offsetof(callFrame{}.nresults))
	_ = uint(unsafe.Offsetof(callFrame{}.nextraargs) - 48)
	_ = uint(48 - unsafe.Offsetof(callFrame{}.nextraargs))
	_ = uint(unsafe.Offsetof(callFrame{}.finish) - 96)
	_ = uint(96 - unsafe.Offsetof(callFrame{}.finish))
	_ = uint(unsafe.Offsetof(callFrame{}.dirty) - 176)
	_ = uint(176 - unsafe.Offsetof(callFrame{}.dirty))
	_ = uint(unsafe.Offsetof(callFrame{}.isTailCall) - 177)
	_ = uint(177 - unsafe.Offsetof(callFrame{}.isTailCall))
	_ = uint(unsafe.Offsetof(callFrame{}.extraArgs) - 183)
	_ = uint(183 - unsafe.Offsetof(callFrame{}.extraArgs))
	_ = uint(unsafe.Offsetof(callFrame{}.oldPC) - 184)
	_ = uint(184 - unsafe.Offsetof(callFrame{}.oldPC))

	// Thread
	_ = uint(unsafe.Offsetof(Thread{}.self) - 8)
	_ = uint(8 - unsafe.Offsetof(Thread{}.self))
	_ = uint(unsafe.Offsetof(Thread{}.stackLen) - 48)
	_ = uint(48 - unsafe.Offsetof(Thread{}.stackLen))
	_ = uint(unsafe.Offsetof(Thread{}.top) - 56)
	_ = uint(56 - unsafe.Offsetof(Thread{}.top))
	_ = uint(unsafe.Offsetof(Thread{}.highStack) - 64)
	_ = uint(64 - unsafe.Offsetof(Thread{}.highStack))
	_ = uint(unsafe.Offsetof(Thread{}.framesCap) - 72)
	_ = uint(72 - unsafe.Offsetof(Thread{}.framesCap))
	_ = uint(unsafe.Offsetof(Thread{}.nframe) - 80)
	_ = uint(80 - unsafe.Offsetof(Thread{}.nframe))
	_ = uint(unsafe.Offsetof(Thread{}.highFrames) - 88)
	_ = uint(88 - unsafe.Offsetof(Thread{}.highFrames))

	// LuaString
	_ = uint(unsafe.Offsetof(LuaString{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Kind) - 4)
	_ = uint(4 - (unsafe.Offsetof(LuaString{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Kind)))
	_ = uint(unsafe.Offsetof(LuaString{}.Len) - 20)
	_ = uint(20 - unsafe.Offsetof(LuaString{}.Len))
	_ = uint(gcKindShortStr - 1)
	_ = uint(1 - gcKindShortStr)

	// Arena constants
	_ = uint(arena.PageSize - (1 << 16))
	_ = uint((1 << 16) - arena.PageSize)
	_ = uint(arena.Align - (1 << 3))
	_ = uint((1 << 3) - arena.Align)
	_ = uint(arena.MarkBlack - (1 << 2))
	_ = uint((1 << 2) - arena.MarkBlack)
	_ = uint(stackSlack - 264)
	_ = uint(264 - stackSlack)
)
