package vm

import (
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yuchanns/moelua/internal/arena"
)

// Zero-Relocation Arena Stdlib Snapshot (Phase 4):
//
// Builds the complete Lua 5.5.1 standard library ONCE via newBootstrapState():
//   - All stdlib short strings live in an immutable process-wide strArena (tagFrozenString,
//     shared read-only across every State, never cloned and never swept).
//   - All stdlib Tables, Shapes, mainThread, stack, frames, and 3 std file Userdatas live
//     inside template.arena with ObjRef links exclusively (ptrdata == 0).
//
// NewState() calls template.arena.CloneSnapshot() to copy the active noscan []byte pages
// verbatim with ZERO pointer relocation, then binds 3 fresh GoRefs handles for stdin/stdout/stderr.

const (
	initFastStackCap = 320 // >= stackSlack (264) + 56 initial registers
	initFastFrameCap = 16
)

type stdTemplate struct {
	// Immutable process-wide frozen stdlib short strings.
	strArena      *arena.Arena
	frozenBuckets []arena.ObjRef
	frozenMask    uint32
	tms           [tmCount]*LuaString
	nextStrID     uint64

	// Read-only Arena snapshot containing all stdlib Tables, Shapes, mainThread, and std file UDs.
	arena  *arena.Arena
	gcSnap arena.CollectorSnapshot

	// Snapshot metadata copied into each cloned State (all ObjRefs remain identical!).
	mainThreadRef arena.ObjRef
	registryRef   arena.ObjRef
	stringMetaRef arena.ObjRef
	fileMetaRef   arena.ObjRef
	pkgRef        arena.ObjRef
	stdinRef      arena.ObjRef
	stdoutRef     arena.ObjRef
	stderrRef     arena.ObjRef

	nextShapeID   uint64
	shapesRootRef arena.ObjRef
	metaRoots     map[metaRootKey]arena.ObjRef
	metaTrans     map[metaTransKey]arena.ObjRef
	transShapes   []arena.ObjRef
	proxyShapes   []arena.ObjRef
	hashedTables  []arena.ObjRef

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
	stdTmplOnce sync.Once
	stdTmpl     *stdTemplate
)

func getStdTemplate() *stdTemplate {
	stdTmplOnce.Do(func() {
		stdTmpl = buildStdTemplate()
	})
	return stdTmpl
}

func buildStdTemplate() *stdTemplate {
	b := newBootstrapState()

	stdinRef := b.defaultInput.Ref()
	stdoutRef := b.defaultOutput.Ref()
	var stderrRef arena.ObjRef
	if env := b.GlobalEnv(); env != nil {
		if ioVal := env.RawGetStr(b, b.intern.Intern("io")); ioVal.IsTable() {
			if se := b.asTable(ioVal).RawGetStr(b, b.intern.Intern("stderr")); se.IsUserdata() {
				stderrRef = se.Ref()
			}
		}
	}

	// Clear GoRefs handles in the template's 3 std file Userdatas (each clone binds its own).
	arena.AsSmall[Userdata](b.arena, stdinRef).dataRef = arena.NullGoHandle
	arena.AsSmall[Userdata](b.arena, stdoutRef).dataRef = arena.NullGoHandle
	arena.AsSmall[Userdata](b.arena, stderrRef).dataRef = arena.NullGoHandle

	// Freeze all bootstrap strings into the process-wide read-only string arena and compact
	// both snapshot arenas so NewState() clones one flat ~48 KiB slab with zero relocation.
	strArena, frozenBuckets, frozenMask := b.intern.FreezeBootstrap()
	strArena.CompactSnapshot()
	b.arena.CompactSnapshot()

	return &stdTemplate{
		strArena:              strArena,
		frozenBuckets:         frozenBuckets,
		frozenMask:            frozenMask,
		tms:                   b.intern.tms,
		nextStrID:             b.intern.nextID,
		arena:                 b.arena,
		gcSnap:                b.collector.Snapshot(),
		mainThreadRef:         b.mainThreadRef,
		registryRef:           b.registryRef,
		stringMetaRef:         b.stringMetaRef,
		fileMetaRef:           b.fileMetaRef,
		pkgRef:                b.pkgRef,
		stdinRef:              stdinRef,
		stdoutRef:             stdoutRef,
		stderrRef:             stderrRef,
		nextShapeID:           b.shapes.nextID,
		shapesRootRef:         b.shapes.rootRef,
		metaRoots:             maps.Clone(b.shapes.metaRoots),
		metaTrans:             maps.Clone(b.shapes.metaTrans),
		transShapes:           slices.Clone(b.shapes.transShapes),
		proxyShapes:           slices.Clone(b.shapes.proxyShapes),
		hashedTables:          slices.Clone(b.hashedTables),
		builtinNext:           b.builtinNext,
		builtinIPairsIter:     b.builtinIPairsIter,
		builtinDebugGetInfo:   b.builtinDebugGetInfo,
		builtinDebugTraceback: b.builtinDebugTraceback,
		builtinDebugGetLocal:  b.builtinDebugGetLocal,
		builtinDebugSetLocal:  b.builtinDebugSetLocal,
		builtinDebugSetHook:   b.builtinDebugSetHook,
		builtinCoroYield:      b.builtinCoroYield,
	}
}

func cloneFromStdTemplate(noEnv bool) *State {
	tmpl := getStdTemplate()
	a := tmpl.arena.CloneSnapshot()
	goRefs := arena.NewGoRefs()

	s := &State{
		arena:         a,
		goRefs:        goRefs,
		mainThreadRef: tmpl.mainThreadRef,
		curRef:        tmpl.mainThreadRef,
		registryRef:   tmpl.registryRef,
		stringMetaRef: tmpl.stringMetaRef,
		fileMetaRef:   tmpl.fileMetaRef,
		pkgRef:        tmpl.pkgRef,
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
	s.typeMetaRefs[4] = s.stringMetaRef
	s.initCollector()
	s.collector.RestoreSnapshot(tmpl.gcSnap)
	s.seedRNG(makeRandomSeed(s), 0)

	// Bind 3 fresh GoRefs handles for stdin, stdout, stderr into the cloned std file Userdatas.
	arena.AsSmall[Userdata](a, tmpl.stdinRef).dataRef = goRefs.Alloc(&luaFile{f: os.Stdin, isStd: true})
	arena.AsSmall[Userdata](a, tmpl.stdoutRef).dataRef = goRefs.Alloc(&luaFile{f: os.Stdout, isStd: true})
	arena.AsSmall[Userdata](a, tmpl.stderrRef).dataRef = goRefs.Alloc(&luaFile{f: os.Stderr, isStd: true})

	stdinVal := userdataRefValue(tmpl.stdinRef)
	stdoutVal := userdataRefValue(tmpl.stdoutRef)
	s.stdinInput = stdinVal
	s.defaultInput = stdinVal
	s.defaultOutput = stdoutVal

	if !noEnv {
		s.applyPackageEnv()
	} else {
		s.registry().RawSetStr(s, s.intern.Intern("LUA_NOENV"), TrueValue())
	}
	return s
}

const (
	defaultLuaPath  = "./?.lua;./?/init.lua"
	defaultLuaCPath = "./?.so;/usr/local/lib/lua/5.5/?.so"
)

// applyPackageEnv applies LUA_PATH_5_5 / LUA_PATH and LUA_CPATH_5_5 / LUA_CPATH
// with Lua 5.5 ';;' default-path substitution. Fast no-op when env vars are unset.
func (s *State) applyPackageEnv() {
	pkg := s.pkgTbl()
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
