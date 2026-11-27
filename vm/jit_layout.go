//go:build (arm64 || amd64) && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

const jitLayoutSupported = true

// jitExitCode values returned by native JIT code through internal/jit/call.Call.
const (
	jitExitDone   uint64 = 0 // Returned below entryDepth; execution complete
	jitExitPoll   uint64 = 1 // Cooperative safepoint/preemption/context poll
	jitExitReload uint64 = 2 // Active frame/closure changed; reload JIT or interpreter
	jitExitCold   uint64 = 3 // Fallback to Go interpreter for current PC
)

// jitPollInterval bounds backward edges and native Lua-to-Lua calls executed inside
// JIT machine code before yielding briefly to Go for GC/preemption safepoints.
const jitPollInterval = 8192

// jitExecCtx is the pointer-free execution context passed by unsafe.Pointer to JIT-compiled
// ARM64/AMD64 machine code. Every backing buffer referenced by uintptr is strongly
// rooted by State, Thread, jitState, and boundProto for the duration of the native call
// and never moves while native code runs (all allocations exit via jitExitCold).
type jitExecCtx struct {
	regs        uintptr // &stack[base] (frame R[0])
	consts      uintptr // &bp.consts[0]
	pagePtrs    uintptr // &s.arena.pagePtrs[0]
	largeSpans  uintptr // &s.arena.largeSpans[0] (24B Go slice headers)
	stack       uintptr // &th.stack[0]
	frames      uintptr // &th.frames[0]
	protos      uintptr // &s.protos[0]
	ic          uintptr // &bp.ic[0]
	icSlots     uintptr // &bp.icSlots[0]
	clUpvals    uintptr // &cl.upvals[0] (0 if nUpvals == 0)
	curBP       uintptr // uintptr(unsafe.Pointer(bp))
	th          uintptr // uintptr(unsafe.Pointer(th))
	entryPC     uintptr // target native PC jumped to by the shared JIT trampoline
	epilogue    uintptr // shared trampoline epilogue address jumped to on exit
	jitEntries  uintptr // &s.jit.entries[0] (0 if empty)
	jitPCTables uintptr // &s.jit.pcTables[0] (0 if empty)
	jitProtoCap int64   // len(s.jit.entries)
	protoEpoch  uint64  // s.protoEpoch
	pc          int64   // current / resume instruction index
	poll        int64   // countdown budget before jitExitPoll
	ci          int64   // active frame index (th.nframe - 1)
	base        int64   // active frame register base
	entryDepth  int64   // return jitExitDone when th.nframe <= entryDepth
	thSelf      uint32  // th.self (arena.ObjRef)
	sqrtIdx     uint32  // staticNative idx for math.sqrt (0 if unregistered)
	absIdx      uint32  // staticNative idx for math.abs (0 if unregistered)
	deoptID     uint32  // side-exit / deopt descriptor index (0 for baseline PC exits)
}

// Compile-time struct field offset constants consumed directly by the ARM64 and AMD64
// JIT code generators so emitted machine code cannot drift from Go struct layouts.
const (
	// jitExecCtx offsets
	jitCtxSize        = int32(unsafe.Sizeof(jitExecCtx{}))
	jitCtxRegs        = int32(unsafe.Offsetof(jitExecCtx{}.regs))
	jitCtxConsts      = int32(unsafe.Offsetof(jitExecCtx{}.consts))
	jitCtxPagePtrs    = int32(unsafe.Offsetof(jitExecCtx{}.pagePtrs))
	jitCtxLargeSpans  = int32(unsafe.Offsetof(jitExecCtx{}.largeSpans))
	jitCtxStack       = int32(unsafe.Offsetof(jitExecCtx{}.stack))
	jitCtxFrames      = int32(unsafe.Offsetof(jitExecCtx{}.frames))
	jitCtxProtos      = int32(unsafe.Offsetof(jitExecCtx{}.protos))
	jitCtxIC          = int32(unsafe.Offsetof(jitExecCtx{}.ic))
	jitCtxICSlots     = int32(unsafe.Offsetof(jitExecCtx{}.icSlots))
	jitCtxClUpvals    = int32(unsafe.Offsetof(jitExecCtx{}.clUpvals))
	jitCtxCurBP       = int32(unsafe.Offsetof(jitExecCtx{}.curBP))
	jitCtxTh          = int32(unsafe.Offsetof(jitExecCtx{}.th))
	jitCtxEntryPC     = int32(unsafe.Offsetof(jitExecCtx{}.entryPC))
	jitCtxEpilogue    = int32(unsafe.Offsetof(jitExecCtx{}.epilogue))
	jitCtxJITEntries  = int32(unsafe.Offsetof(jitExecCtx{}.jitEntries))
	jitCtxJITPCTables = int32(unsafe.Offsetof(jitExecCtx{}.jitPCTables))
	jitCtxJITProtoCap = int32(unsafe.Offsetof(jitExecCtx{}.jitProtoCap))
	jitCtxProtoEpoch  = int32(unsafe.Offsetof(jitExecCtx{}.protoEpoch))
	jitCtxPC          = int32(unsafe.Offsetof(jitExecCtx{}.pc))
	jitCtxPoll        = int32(unsafe.Offsetof(jitExecCtx{}.poll))
	jitCtxCI          = int32(unsafe.Offsetof(jitExecCtx{}.ci))
	jitCtxBase        = int32(unsafe.Offsetof(jitExecCtx{}.base))
	jitCtxEntryDepth  = int32(unsafe.Offsetof(jitExecCtx{}.entryDepth))
	jitCtxThSelf      = int32(unsafe.Offsetof(jitExecCtx{}.thSelf))
	jitCtxSqrtIdx     = int32(unsafe.Offsetof(jitExecCtx{}.sqrtIdx))
	jitCtxAbsIdx      = int32(unsafe.Offsetof(jitExecCtx{}.absIdx))
	jitCtxDeoptID     = int32(unsafe.Offsetof(jitExecCtx{}.deoptID))

	// Value layout
	jitValSize = int32(unsafe.Sizeof(Value{}))
	jitValBits = int32(unsafe.Offsetof(Value{}.bits))
	jitValTag  = int32(unsafe.Offsetof(Value{}.tag))

	// Table layout
	jitTblSize        = int32(unsafe.Sizeof(Table{}))
	jitTblMark        = int32(unsafe.Offsetof(Table{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Mark))
	jitTblFlags       = int32(unsafe.Offsetof(Table{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Flags))
	jitTblSelf        = int32(unsafe.Offsetof(Table{}.self))
	jitTblShapeRef    = int32(unsafe.Offsetof(Table{}.shapeRef))
	jitTblMetaRef     = int32(unsafe.Offsetof(Table{}.metaRef))
	jitTblHashRef     = int32(unsafe.Offsetof(Table{}.hashRef))
	jitTblArrRef      = int32(unsafe.Offsetof(Table{}.arrRef))
	jitTblSlotsRef    = int32(unsafe.Offsetof(Table{}.slotsRef))
	jitTblArrLen      = int32(unsafe.Offsetof(Table{}.arrLen))
	jitTblArrCap      = int32(unsafe.Offsetof(Table{}.arrCap))
	jitTblSlotsLen    = int32(unsafe.Offsetof(Table{}.slotsLen))
	jitTblSlotsCap    = int32(unsafe.Offsetof(Table{}.slotsCap))
	jitTblArrKind     = int32(unsafe.Offsetof(Table{}.arrKind))
	jitTblTMFlags     = int32(unsafe.Offsetof(Table{}.tmFlags))
	jitTblInlineSlots = int32(unsafe.Offsetof(Table{}.inlineSlots))

	// tableHash & hashNode layout
	jitThashNodesRef = int32(unsafe.Offsetof(tableHash{}.nodesRef))
	jitThashDictGen  = int32(unsafe.Offsetof(tableHash{}.dictGen))
	jitHNodeSize     = int32(unsafe.Sizeof(hashNode{}))
	jitHNodeVal      = int32(unsafe.Offsetof(hashNode{}.val))

	// Closure layout
	jitClSize      = int32(unsafe.Sizeof(Closure{}))
	jitClSelf      = int32(unsafe.Offsetof(Closure{}.self))
	jitClProtoRef  = int32(unsafe.Offsetof(Closure{}.protoRef))
	jitClUpvalsRef = int32(unsafe.Offsetof(Closure{}.upvalsRef))
	jitClProtoIdx  = int32(unsafe.Offsetof(Closure{}.protoIdx))
	jitClNUpvals   = int32(unsafe.Offsetof(Closure{}.nUpvals))

	// Upvalue layout
	jitUvSize     = int32(unsafe.Sizeof(Upvalue{}))
	jitUvMark     = int32(unsafe.Offsetof(Upvalue{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Mark))
	jitUvOwnerRef = int32(unsafe.Offsetof(Upvalue{}.ownerRef))
	jitUvClosed   = int32(unsafe.Offsetof(Upvalue{}.closed))
	jitUvStackIdx = int32(unsafe.Offsetof(Upvalue{}.stackIdx))

	// ICEntry & polyEntry layout
	jitICSize        = int32(unsafe.Sizeof(ICEntry{}))
	jitICVal         = int32(unsafe.Offsetof(ICEntry{}.val))
	jitICEpoch       = int32(unsafe.Offsetof(ICEntry{}.epoch))
	jitICShapeRef    = int32(unsafe.Offsetof(ICEntry{}.shapeRef))
	jitICTargetShape = int32(unsafe.Offsetof(ICEntry{}.targetShapeRef))
	jitICHolderRef   = int32(unsafe.Offsetof(ICEntry{}.holderRef))
	jitICPolyRef     = int32(unsafe.Offsetof(ICEntry{}.polyRef))
	jitICSlot        = int32(unsafe.Offsetof(ICEntry{}.slot))
	jitICKind        = int32(unsafe.Offsetof(ICEntry{}.kind))
	jitICNoMeta      = int32(unsafe.Offsetof(ICEntry{}.noMeta))
	jitICProtoAbsent = int32(unsafe.Offsetof(ICEntry{}.protoAbsent))
	jitICPolyCount   = int32(unsafe.Offsetof(ICEntry{}.polyCount))

	jitPolySize        = int32(unsafe.Sizeof(polyEntry{}))
	jitPolyVal         = int32(unsafe.Offsetof(polyEntry{}.val))
	jitPolyEpoch       = int32(unsafe.Offsetof(polyEntry{}.epoch))
	jitPolyShapeRef    = int32(unsafe.Offsetof(polyEntry{}.shapeRef))
	jitPolyTargetShape = int32(unsafe.Offsetof(polyEntry{}.targetShapeRef))
	jitPolySlot        = int32(unsafe.Offsetof(polyEntry{}.slot))
	jitPolyKind        = int32(unsafe.Offsetof(polyEntry{}.kind))
	jitPolyNoMeta      = int32(unsafe.Offsetof(polyEntry{}.noMeta))
	jitPolyProtoAbsent = int32(unsafe.Offsetof(polyEntry{}.protoAbsent))

	// boundProto layout
	jitBPConsts    = int32(unsafe.Offsetof(boundProto{}.consts))
	jitBPICSlots   = int32(unsafe.Offsetof(boundProto{}.icSlots))
	jitBPIC        = int32(unsafe.Offsetof(boundProto{}.ic))
	jitBPNumParams = int32(unsafe.Offsetof(boundProto{}.numParams))
	jitBPMaxStack  = int32(unsafe.Offsetof(boundProto{}.maxStack))
	jitBPIsVararg  = int32(unsafe.Offsetof(boundProto{}.isVararg))

	// callFrame layout
	jitCFSize         = int32(unsafe.Sizeof(callFrame{}))
	jitCFKind         = int32(unsafe.Offsetof(callFrame{}.kind))
	jitCFClRef        = int32(unsafe.Offsetof(callFrame{}.clRef))
	jitCFVarargTabRef = int32(unsafe.Offsetof(callFrame{}.varargTabRef))
	jitCFBase         = int32(unsafe.Offsetof(callFrame{}.base))
	jitCFFuncIdx      = int32(unsafe.Offsetof(callFrame{}.funcIdx))
	jitCFPC           = int32(unsafe.Offsetof(callFrame{}.pc))
	jitCFNResults     = int32(unsafe.Offsetof(callFrame{}.nresults))
	jitCFNExtraArgs   = int32(unsafe.Offsetof(callFrame{}.nextraargs))
	jitCFFinish       = int32(unsafe.Offsetof(callFrame{}.finish))
	jitCFDirty        = int32(unsafe.Offsetof(callFrame{}.dirty))
	jitCFIsTailCall   = int32(unsafe.Offsetof(callFrame{}.isTailCall))
	jitCFExtraArgs    = int32(unsafe.Offsetof(callFrame{}.extraArgs))
	jitCFOldPC        = int32(unsafe.Offsetof(callFrame{}.oldPC))

	// Thread layout
	jitThSelf       = int32(unsafe.Offsetof(Thread{}.self))
	jitThStackLen   = int32(unsafe.Offsetof(Thread{}.stackLen))
	jitThTop        = int32(unsafe.Offsetof(Thread{}.top))
	jitThHighStack  = int32(unsafe.Offsetof(Thread{}.highStack))
	jitThFramesCap  = int32(unsafe.Offsetof(Thread{}.framesCap))
	jitThNFrame     = int32(unsafe.Offsetof(Thread{}.nframe))
	jitThHighFrames = int32(unsafe.Offsetof(Thread{}.highFrames))

	// LuaString layout
	jitStrKind = int32(unsafe.Offsetof(LuaString{}.hdr) + unsafe.Offsetof(arena.GCHeader{}.Kind))
	jitStrLen  = int32(unsafe.Offsetof(LuaString{}.Len))

	// Arena ObjRef & slab constants
	jitArenaWordBits   = 13
	jitArenaWordMask   = (1 << jitArenaWordBits) - 1
	jitArenaAlignShift = 3
	jitArenaLargeBit   = 31
	jitMarkBlackBit    = 2
	jitSliceHeaderSize = 24
)

// Compile-time invariants required by JIT code generation.
const (
	_ = uint(jitValSize - 16)
	_ = uint(16 - jitValSize)
	_ = uint(jitValBits - 0)
	_ = uint(jitValTag - 8)
	_ = uint(tagInt - 0)
	_ = uint(tagFloat - 1)
	_ = uint(arena.PageSize - (1 << 16))
	_ = uint((1 << 16) - arena.PageSize)
	_ = uint(arena.Align - (1 << jitArenaAlignShift))
	_ = uint((1 << jitArenaAlignShift) - arena.Align)
	_ = uint(arena.MarkBlack - (1 << jitMarkBlackBit))
	_ = uint((1 << jitMarkBlackBit) - arena.MarkBlack)
	_ = uint(unsafe.Sizeof([]byte(nil)) - jitSliceHeaderSize)
	_ = uint(jitSliceHeaderSize - unsafe.Sizeof([]byte(nil)))
	_ = uint(jitCtxSize % 8)
)
