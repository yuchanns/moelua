package vm

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/arena"
)

// Zero-Relocation Arena Template Snapshot:
//
// Builds a Lua 5.5.1 State ONCE with selected standard libraries (LibMask) plus optional
// host setup (Preload, RequireLib, RegisterFunc, AddFS, Lua modules, metatables):
//   - All bootstrap strings live in an immutable read-only strArena (tagFrozenString,
//     shared across every cloned State, never cloned and never swept).
//   - All Tables, Shapes, Closures, ArenaProtos, ArenaNatives, mainThread, stack, frames,
//     and std file Userdatas live inside tmpl.arena with ObjRef links exclusively (ptrdata == 0).
//
// Clone() calls tmpl.arena.CloneSnapshot() to copy the active noscan []byte pages verbatim
// with ZERO pointer relocation, clones GoRefs for any custom Go callbacks, rebinds ArenaProto
// slice views if Lua chunks were loaded during setup, and binds fresh stdin/stdout/stderr
// handles when LibIO is enabled.

const (
	initFastStackCap = 320 // >= stackSlack (264) + 56 initial registers
	initFastFrameCap = 16
)

type tmplProtoDesc struct {
	proto    *bytecode.Proto
	arenaRef arena.ObjRef
	protoIdx uint32
	goBytes  int64
}

// Template is an immutable, compacted Arena snapshot of a configured Lua 5.5.1 environment.
// Multiple goroutines may call (*Template).Clone concurrently to spawn fully isolated States
// in microseconds without re-running library registration.
type Template struct {
	// Immutable read-only frozen bootstrap strings shared across cloned States.
	strArena      *arena.Arena
	frozenBuckets []arena.ObjRef
	frozenMask    uint32
	tms           [tmCount]*LuaString
	nextStrID     uint64

	// Read-only Arena & GoRefs snapshots cloned per State.
	arena            *arena.Arena
	goRefs           *arena.GoRefs
	hasMutableGoRefs bool
	gcSnap           arena.CollectorSnapshot

	// Snapshot metadata copied into each cloned State (all ObjRefs remain identical!).
	mainThreadRef arena.ObjRef
	registryRef   arena.ObjRef
	stringMetaRef arena.ObjRef
	fileMetaRef   arena.ObjRef
	pkgRef        arena.ObjRef
	stdinRef      arena.ObjRef
	stdoutRef     arena.ObjRef
	stderrRef     arena.ObjRef
	defaultInput  Value
	defaultOutput Value
	typeMetaRefs  [9]arena.ObjRef
	openedLibs    LibMask
	pkgAllowOS    bool
	fsSearchers   int
	protoEpoch    uint64
	extraMemBytes int64

	protos       []*tmplProtoDesc
	freeProtoIDs []uint32

	nextShapeID     uint64
	shapesRootRef   arena.ObjRef
	metaRoots       map[metaRootKey]arena.ObjRef
	metaTrans       map[metaTransKey]arena.ObjRef
	transShapes     []arena.ObjRef
	proxyShapes     []arena.ObjRef
	hashedTables    []arena.ObjRef
	twups           []arena.ObjRef
	hasDeadHashKeys bool
	warnOn          bool
	immUpvalTokens  map[uint32][]uint64

	builtinNext           *NativeFunc
	builtinIPairsIter     *NativeFunc
	builtinDebugGetInfo   *NativeFunc
	builtinDebugTraceback *NativeFunc
	builtinDebugGetLocal  *NativeFunc
	builtinDebugSetLocal  *NativeFunc
	builtinDebugSetHook   *NativeFunc
	builtinCoroYield      *NativeFunc
}

var (
	stdTmplAllOnce sync.Once
	stdTmplAll     *Template

	stdTmplMu    sync.RWMutex
	stdTmplCache = make(map[LibMask]*Template, 8)
)

const maxCachedLibTemplates = 32

func getStdTemplate(mask LibMask) *Template {
	mask = normalizeLibMask(mask)
	if mask == LibAll {
		stdTmplAllOnce.Do(func() {
			t, err := BuildTemplate(LibAll, nil)
			if err != nil {
				panic(err)
			}
			stdTmplAll = t
		})
		return stdTmplAll
	}
	stdTmplMu.RLock()
	if t, ok := stdTmplCache[mask]; ok {
		stdTmplMu.RUnlock()
		return t
	}
	stdTmplMu.RUnlock()

	t, err := BuildTemplate(mask, nil)
	if err != nil {
		panic(err)
	}
	stdTmplMu.Lock()
	if existing, ok := stdTmplCache[mask]; ok {
		stdTmplMu.Unlock()
		return existing
	}
	if len(stdTmplCache) < maxCachedLibTemplates {
		stdTmplCache[mask] = t
	}
	stdTmplMu.Unlock()
	return t
}

// BuildTemplate constructs a fresh bootstrap State with the selected standard libraries,
// runs optional host setup (e.g. Preload, RequireLib, RegisterFunc, AddFS, Lua modules),
// collects temporary setup garbage, freezes bootstrap strings, and compacts the Arena
// snapshot into a concurrency-safe reusable *Template.
func BuildTemplate(mask LibMask, setup func(*State) error) (*Template, error) {
	b := newBootstrapStateWithLibs(mask)
	if setup != nil {
		if err := setup(b); err != nil {
			b.Close()
			return nil, err
		}
		if b.abortErr != nil {
			err := b.abortErr
			b.Close()
			return nil, err
		}
		if err := b.ConsumeMemErr(); err != nil {
			b.Close()
			return nil, err
		}
		b.compactOpenFiles()
		if len(b.openFiles) > 0 {
			b.Close()
			return nil, fmt.Errorf("moelua: template setup left %d open file handle(s); close files before finalizing template", len(b.openFiles))
		}
		if len(b.pinnedRefs) > 0 {
			b.Close()
			return nil, fmt.Errorf("moelua: template setup left %d pinned Handle(s); release pins before finalizing template", len(b.pinnedRefs))
		}
		if th := b.cur(); th != nil && (th.nframe != 0 || th.hookMask != 0) {
			b.Close()
			return nil, fmt.Errorf("moelua: template setup left active call frames or debug hooks")
		}
		// Clear transient setup controls and normalize GC to incremental mode before collecting
		// temporary setup garbage so CollectorSnapshot starts cleanly in GCSPause.
		b.SetContext(nil)
		b.SetInstructionLimit(0)
		b.SetMemoryLimit(0)
		b.closeJIT()
		b.collector.SetMode(arena.ModeIncremental)
		b.CollectFull()
		b.compactOpenFiles()
		if len(b.openFiles) > 0 {
			b.Close()
			return nil, fmt.Errorf("moelua: template setup left %d open file handle(s) after GC", len(b.openFiles))
		}
		if len(b.pinnedRefs) > 0 {
			b.Close()
			return nil, fmt.Errorf("moelua: template setup left %d pinned Handle(s) after GC", len(b.pinnedRefs))
		}
		if th := b.cur(); th != nil && (th.nframe != 0 || th.hookMask != 0) {
			b.Close()
			return nil, fmt.Errorf("moelua: template setup left active call frames or debug hooks after GC")
		}
	}

	// Clear template GoRefs handles on std file Userdatas so each cloned State binds fresh
	// os.Stdin/os.Stdout/os.Stderr handles without retaining template-owned *luaFile wrappers.
	clearStdFileRef := func(ref arena.ObjRef) arena.ObjRef {
		if ref == arena.NullRef || !b.arena.IsLive(ref) {
			return arena.NullRef
		}
		u := arena.AsSmall[Userdata](b.arena, ref)
		if !u.dataRef.IsNull() {
			if v, ok := b.goRefs.Get(u.dataRef); ok {
				if lf, ok := v.(*luaFile); ok && lf != nil && lf.w != nil {
					_ = lf.flushWriter()
					b.subExtraMem(int64(lf.w.Size()))
				}
			}
			b.goRefs.Free(u.dataRef)
			u.dataRef = arena.NullGoHandle
		}
		return ref
	}
	stdinRef := clearStdFileRef(b.stdinRef)
	stdoutRef := clearStdFileRef(b.stdoutRef)
	stderrRef := clearStdFileRef(b.stderrRef)

	// Snapshot GoRefs, detaching bootstrap State references from mutable built-in payloads
	// (such as *gmatchState) and recording whether Clone() must clone per-State payloads.
	hasMutableGoRefs := false
	tmplGoRefs := b.goRefs.CloneWith(func(v any) any {
		switch x := v.(type) {
		case *gmatchState:
			hasMutableGoRefs = true
			if x == nil {
				return nil
			}
			return &gmatchState{
				it:     x.it.CloneWithCancel(nil),
				srcLen: x.srcLen,
			}
		case *luaFile:
			hasMutableGoRefs = true
			return &luaFile{closed: true}
		default:
			return v
		}
	})

	// Capture surviving boundProto descriptors (non-empty when setup defined Lua closures).
	var protos []*tmplProtoDesc
	if len(b.protos) > 1 {
		protos = make([]*tmplProtoDesc, len(b.protos))
		for i := 1; i < len(b.protos); i++ {
			if bp := b.protos[i]; bp != nil {
				protos[i] = &tmplProtoDesc{
					proto:    bp.proto,
					arenaRef: bp.arenaRef,
					protoIdx: bp.protoIdx,
					goBytes:  bp.goBytes,
				}
			}
		}
	}

	// Freeze all bootstrap strings into the template's read-only string arena and compact
	// both snapshot arenas so Clone() copies one flat slab with zero pointer relocation.
	strArena, frozenBuckets, frozenMask := b.intern.FreezeBootstrap()
	strArena.CompactSnapshot()
	var tms [tmCount]*LuaString
	for i := range tmCount {
		if b.intern.tms[i] != nil {
			tms[i] = arena.AsFast[LuaString](strArena, b.intern.tms[i].self)
		}
	}
	b.arena.CompactSnapshot()
	b.closeJIT()
	b.SetHost(nil)

	return &Template{
		strArena:              strArena,
		frozenBuckets:         frozenBuckets,
		frozenMask:            frozenMask,
		tms:                   tms,
		nextStrID:             b.intern.nextID,
		arena:                 b.arena,
		goRefs:                tmplGoRefs,
		hasMutableGoRefs:      hasMutableGoRefs,
		gcSnap:                b.collector.Snapshot(),
		mainThreadRef:         b.mainThreadRef,
		registryRef:           b.registryRef,
		stringMetaRef:         b.stringMetaRef,
		fileMetaRef:           b.fileMetaRef,
		pkgRef:                b.pkgRef,
		stdinRef:              stdinRef,
		stdoutRef:             stdoutRef,
		stderrRef:             stderrRef,
		defaultInput:          b.defaultInput,
		defaultOutput:         b.defaultOutput,
		typeMetaRefs:          b.typeMetaRefs,
		openedLibs:            b.openedLibs,
		pkgAllowOS:            b.pkgAllowOS,
		fsSearchers:           b.fsSearchers,
		protoEpoch:            b.protoEpoch,
		extraMemBytes:         b.extraMemBytes,
		protos:                protos,
		freeProtoIDs:          slices.Clone(b.freeProtoIDs),
		nextShapeID:           b.shapes.nextID,
		shapesRootRef:         b.shapes.rootRef,
		metaRoots:             maps.Clone(b.shapes.metaRoots),
		metaTrans:             maps.Clone(b.shapes.metaTrans),
		transShapes:           slices.Clone(b.shapes.transShapes),
		proxyShapes:           slices.Clone(b.shapes.proxyShapes),
		hashedTables:          slices.Clone(b.hashedTables),
		twups:                 slices.Clone(b.twups),
		hasDeadHashKeys:       b.hasDeadHashKeys,
		warnOn:                b.warnOn,
		immUpvalTokens:        cloneUpvalTokens(b.immUpvalTokens),
		builtinNext:           b.builtinNext,
		builtinIPairsIter:     b.builtinIPairsIter,
		builtinDebugGetInfo:   b.builtinDebugGetInfo,
		builtinDebugTraceback: b.builtinDebugTraceback,
		builtinDebugGetLocal:  b.builtinDebugGetLocal,
		builtinDebugSetLocal:  b.builtinDebugSetLocal,
		builtinDebugSetHook:   b.builtinDebugSetHook,
		builtinCoroYield:      b.builtinCoroYield,
	}, nil
}

// Clone instantiates an isolated Lua 5.5.1 State from tmpl via zero-relocation Arena copy.
// Safe to call concurrently from multiple goroutines.
func (tmpl *Template) Clone(noEnv bool) *State {
	a := tmpl.arena.CloneSnapshot()

	var protos []*boundProto
	if len(tmpl.protos) > 1 {
		protos = make([]*boundProto, len(tmpl.protos))
		for i := 1; i < len(tmpl.protos); i++ {
			tp := tmpl.protos[i]
			if tp == nil {
				continue
			}
			ap := arena.AsSmall[ArenaProto](a, tp.arenaRef)
			var consts []Value
			if ap.constsRef != arena.NullRef && ap.nConsts > 0 {
				consts = arena.SliceFast[Value](a, ap.constsRef, 0, int(ap.nConsts))
			}
			var ic []ICEntry
			if ap.icRef != arena.NullRef && ap.nIC > 0 {
				ic = arena.SliceFast[ICEntry](a, ap.icRef, 0, int(ap.nIC))
				clear(ic)
			}
			var sub []arena.ObjRef
			if ap.subRef != arena.NullRef && ap.nSub > 0 {
				sub = arena.SliceFast[arena.ObjRef](a, ap.subRef, 0, int(ap.nSub))
			}
			protos[i] = &boundProto{
				proto:     tp.proto,
				insns:     codeOf(tp.proto.Code),
				consts:    consts,
				icSlots:   tp.proto.ICSlot,
				ic:        ic,
				sub:       sub,
				arenaRef:  tp.arenaRef,
				protoIdx:  tp.protoIdx,
				goBytes:   tp.goBytes,
				numParams: tp.proto.NumParams,
				maxStack:  tp.proto.MaxStackSize,
				isVararg:  tp.proto.IsVararg(),
			}
		}
	} else {
		protos = []*boundProto{nil}
	}

	s := &State{
		arena:         a,
		mainThreadRef: tmpl.mainThreadRef,
		curRef:        tmpl.mainThreadRef,
		registryRef:   tmpl.registryRef,
		stringMetaRef: tmpl.stringMetaRef,
		fileMetaRef:   tmpl.fileMetaRef,
		pkgRef:        tmpl.pkgRef,
		stdinRef:      tmpl.stdinRef,
		stdoutRef:     tmpl.stdoutRef,
		stderrRef:     tmpl.stderrRef,
		typeMetaRefs:  tmpl.typeMetaRefs,
		openedLibs:    tmpl.openedLibs,
		pkgAllowOS:    tmpl.pkgAllowOS,
		fsSearchers:   tmpl.fsSearchers,
		protoEpoch:    tmpl.protoEpoch,
		protos:        protos,
		freeProtoIDs:  slices.Clone(tmpl.freeProtoIDs),
		extraMemBytes: tmpl.extraMemBytes,
		intern: stringInterner{
			seed:          globalStrSeed,
			nextID:        tmpl.nextStrID,
			frozenArena:   tmpl.strArena,
			frozenBuckets: tmpl.frozenBuckets,
			frozenMask:    tmpl.frozenMask,
			tms:           tmpl.tms,
		},
		shapes: shapeRegistry{
			nextID:      tmpl.nextShapeID,
			rootRef:     tmpl.shapesRootRef,
			metaRoots:   maps.Clone(tmpl.metaRoots),
			metaTrans:   maps.Clone(tmpl.metaTrans),
			transShapes: slices.Clone(tmpl.transShapes),
			proxyShapes: slices.Clone(tmpl.proxyShapes),
		},
		hashedTables:          slices.Clone(tmpl.hashedTables),
		twups:                 slices.Clone(tmpl.twups),
		hasDeadHashKeys:       tmpl.hasDeadHashKeys,
		warnOn:                tmpl.warnOn,
		immUpvalTokens:        cloneUpvalTokens(tmpl.immUpvalTokens),
		nextRefID:             1,
		out:                   os.Stdout,
		errOut:                os.Stderr,
		startTime:             time.Now(),
		compilerFn:            defaultCompileFunc,
		compilerReaderFn:      defaultCompileReaderFunc,
		builtinNext:           tmpl.builtinNext,
		builtinIPairsIter:     tmpl.builtinIPairsIter,
		builtinDebugGetInfo:   tmpl.builtinDebugGetInfo,
		builtinDebugTraceback: tmpl.builtinDebugTraceback,
		builtinDebugGetLocal:  tmpl.builtinDebugGetLocal,
		builtinDebugSetLocal:  tmpl.builtinDebugSetLocal,
		builtinDebugSetHook:   tmpl.builtinDebugSetHook,
		builtinCoroYield:      tmpl.builtinCoroYield,
	}
	var goRefs *arena.GoRefs
	if tmpl.hasMutableGoRefs {
		goRefs = tmpl.goRefs.CloneWith(func(v any) any {
			switch x := v.(type) {
			case *gmatchState:
				if x == nil {
					return nil
				}
				return &gmatchState{
					it:     x.it.CloneWithCancel(s.dynamicNativeCancelFunc()),
					srcLen: x.srcLen,
				}
			case *luaFile:
				return &luaFile{closed: true}
			default:
				return v
			}
		})
	} else {
		goRefs = tmpl.goRefs.Clone()
	}
	s.goRefs = goRefs

	s.initCollector()
	s.collector.RestoreSnapshot(tmpl.gcSnap)
	s.seedRNG(makeRandomSeed(s), 0)

	// Bind fresh GoRefs handles for stdin, stdout, stderr when LibIO is present in tmpl,
	// and preserve whichever default input/output streams setup configured (e.g. io.output(io.stderr)).
	if tmpl.stdinRef != arena.NullRef {
		arena.AsSmall[Userdata](a, tmpl.stdinRef).dataRef = goRefs.Alloc(&luaFile{f: os.Stdin, isStd: true})
		s.stdinInput = userdataRefValue(tmpl.stdinRef)
	}
	if tmpl.stdoutRef != arena.NullRef {
		arena.AsSmall[Userdata](a, tmpl.stdoutRef).dataRef = goRefs.Alloc(&luaFile{f: os.Stdout, isStd: true})
	}
	if tmpl.stderrRef != arena.NullRef {
		arena.AsSmall[Userdata](a, tmpl.stderrRef).dataRef = goRefs.Alloc(&luaFile{f: os.Stderr, isStd: true})
	}
	s.defaultInput = tmpl.defaultInput
	s.defaultOutput = tmpl.defaultOutput

	if !noEnv {
		s.applyPackageEnv()
	} else {
		s.registry().RawSetStr(s, s.intern.Intern("LUA_NOENV"), TrueValue())
	}
	return s
}

func cloneUpvalTokens(src map[uint32][]uint64) map[uint32][]uint64 {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[uint32][]uint64, len(src))
	for k, v := range src {
		dst[k] = slices.Clone(v)
	}
	return dst
}

func cloneFromStdTemplate(mask LibMask, noEnv bool) *State {
	return getStdTemplate(mask).Clone(noEnv)
}

const (
	defaultLuaPath  = "./?.lua;./?/init.lua"
	defaultLuaCPath = "./?.so;/usr/local/lib/lua/5.5/?.so"
)

// applyPackageEnv applies LUA_PATH_5_5 / LUA_PATH and LUA_CPATH_5_5 / LUA_CPATH
// with Lua 5.5 ';;' default-path substitution. Fast no-op when OS package is disabled
// or env vars are unset.
func (s *State) applyPackageEnv() {
	if !s.pkgAllowOS {
		return
	}
	pkg := s.pkgTbl()
	if pkg == nil {
		return
	}
	if v, ok := os.LookupEnv("LUA_PATH_5_5"); ok {
		pkg.RawSetStr(s, s.intern.Intern("path"), StringValue(s.intern.Intern(expandDefaultPath(v, defaultLuaPath))))
	} else if v, ok := os.LookupEnv("LUA_PATH"); ok {
		pkg.RawSetStr(s, s.intern.Intern("path"), StringValue(s.intern.Intern(expandDefaultPath(v, defaultLuaPath))))
	}
	if v, ok := os.LookupEnv("LUA_CPATH_5_5"); ok {
		pkg.RawSetStr(s, s.intern.Intern("cpath"), StringValue(s.intern.Intern(expandDefaultPath(v, defaultLuaCPath))))
	} else if v, ok := os.LookupEnv("LUA_CPATH"); ok {
		pkg.RawSetStr(s, s.intern.Intern("cpath"), StringValue(s.intern.Intern(expandDefaultPath(v, defaultLuaCPath))))
	}
}

func expandDefaultPath(val, dflt string) string {
	idx := strings.Index(val, ";;")
	if idx < 0 {
		return val
	}
	var sb strings.Builder
	if idx > 0 {
		sb.WriteString(val[:idx])
		sb.WriteByte(';')
	}
	sb.WriteString(dflt)
	if idx+2 < len(val) {
		sb.WriteByte(';')
		sb.WriteString(val[idx+2:])
	}
	return sb.String()
}
