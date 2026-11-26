package vm

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yuchanns/moelua/internal/arena"
)

// Frozen Stdlib String Arena Template (Step 5c):
//
// All stdlib short strings live in one process-wide immutable strArena (tagFrozenString),
// shared read-only across every State. Full zero-relocation Arena.CloneSnapshot() for
// stdlib Tables, Shapes, and mainThread lands in Step 5d once those structs move to Arena.

const (
	initFastStackCap = 320 // >= stackSlack (264) + 56 initial registers
	initFastFrameCap = 16
)

type stdTemplate struct {
	strArena      *arena.Arena
	frozenBuckets []arena.ObjRef
	frozenMask    uint32
	tms           [tmCount]*LuaString
	nextStrID     uint64

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
	strArena, frozenBuckets, frozenMask := b.intern.FreezeBootstrap()
	tmpl := &stdTemplate{
		strArena:              strArena,
		frozenBuckets:         frozenBuckets,
		frozenMask:            frozenMask,
		tms:                   b.intern.tms,
		nextStrID:             b.intern.nextID,
		builtinNext:           b.builtinNext,
		builtinIPairsIter:     b.builtinIPairsIter,
		builtinDebugGetInfo:   b.builtinDebugGetInfo,
		builtinDebugTraceback: b.builtinDebugTraceback,
		builtinDebugGetLocal:  b.builtinDebugGetLocal,
		builtinDebugSetLocal:  b.builtinDebugSetLocal,
		builtinDebugSetHook:   b.builtinDebugSetHook,
		builtinCoroYield:      b.builtinCoroYield,
	}
	b.Close()
	return tmpl
}

func cloneFromStdTemplate(noEnv bool) *State {
	tmpl := getStdTemplate()
	a := arena.New()
	s := &State{
		arena:  a,
		goRefs: arena.NewGoRefs(),
		intern: stringInterner{
			seed:          globalStrSeed,
			nextID:        tmpl.nextStrID,
			frozenArena:   tmpl.strArena,
			frozenBuckets: tmpl.frozenBuckets,
			frozenMask:    tmpl.frozenMask,
			tms:           tmpl.tms,
		},
		shapes:           newShapeRegistry(),
		protos:           []*boundProto{nil},
		tables:           []*Table{nil},
		nextRefID:        1,
		out:              os.Stdout,
		errOut:           os.Stderr,
		startTime:        time.Now(),
		compilerFn:       defaultCompileFunc,
		compilerReaderFn: defaultCompileReaderFunc,
	}
	s.initCollector()
	s.seedRNG(makeRandomSeed(s), 0)

	th, err := s.newThread(initFastStackCap, initFastFrameCap)
	if err != nil {
		panic(err)
	}
	th.nny = 1 // main thread is never yieldable (L->nny = 1)
	s.mainThreadRef = th.self
	s.curRef = th.self

	env := s.NewDictTable(64)
	reg := s.NewDictTable(16)
	s.registryRef = reg.self
	reg.RawSetInt(s, ridxRef, FalseValue())
	reg.RawSetInt(s, ridxGlobals, TableValue(env))
	reg.RawSetInt(s, ridxMainThread, ThreadValue(th))
	s.initStdlib()
	s.builtinNext = tmpl.builtinNext
	s.builtinIPairsIter = tmpl.builtinIPairsIter
	s.builtinDebugGetInfo = tmpl.builtinDebugGetInfo
	s.builtinDebugTraceback = tmpl.builtinDebugTraceback
	s.builtinDebugGetLocal = tmpl.builtinDebugGetLocal
	s.builtinDebugSetLocal = tmpl.builtinDebugSetLocal
	s.builtinDebugSetHook = tmpl.builtinDebugSetHook
	s.builtinCoroYield = tmpl.builtinCoroYield
	s.shapes.frozenMaxID = s.shapes.nextID
	s.shapeMemBytes = 0

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
