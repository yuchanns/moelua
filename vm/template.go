package vm

import (
	"os"
	"strings"
	"sync"
	"time"
)

// Frozen Stdlib Template (Phase 2):
//
// Builds the complete Lua 5.5.1 standard library ONCE via newBootstrapState(),
// freezes all stdlib Shapes and short strings as read-only process-wide snapshots,
// and packs all ~20 stdlib tables into flat slabs so NewState() clones a fully
// isolated State in 4 heap allocations (~3.25 µs vs ~34.5 µs uncached).

const (
	maxStdTables     = 24
	initFastStackCap = 320 // >= stackSlack (264) + 56 initial registers
	initFastFrameCap = 16
)

type tmplRelocKind uint8

const (
	relocTable tmplRelocKind = iota
	relocMainThread
	relocStdIn
	relocStdOut
	relocStdErr
)

type tmplRelocDest uint8

const (
	destSlot tmplRelocDest = iota
	destNodeKey
	destNodeVal
	destArr
)

type tmplReloc struct {
	dest   tmplRelocDest
	idx    int
	kind   tmplRelocKind
	target int // table index for relocTable
}

type tmplTableDesc struct {
	base     Table     // shallow scalar/shape fields (slices/ptrs cleared)
	hashBase tableHash // shallow hash metadata (nodes cleared)
	slotOff  int
	slotLen  int
	nodeOff  int
	nodeLen  int
	arrOff   int
	arrLen   int
	metaIdx  int // -1 if nil
}

type stdTemplate struct {
	short     map[string]*LuaString
	tms       [tmCount]*LuaString
	nextStrID uint32

	nextShapeID uint32

	builtinNext         *NativeFunc
	builtinIPairsIter   *NativeFunc
	builtinDebugGetInfo *NativeFunc
	builtinCoroYield    *NativeFunc

	tables []tmplTableDesc
	slots  []Value
	nodes  []hashNode
	arr    []Value
	relocs []tmplReloc

	envIdx        int
	registryIdx   int
	stringMetaIdx int
	fileMetaIdx   int
	pkgIdx        int
	loadedIdx     int
	preloadIdx    int

	nilStack [initFastStackCap]Value
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

func freezeShapeChain(sh *Shape) {
	for cur := sh; cur != nil; cur = cur.parent {
		cur.frozen = true
	}
}

func buildStdTemplate() *stdTemplate {
	b := newBootstrapState()

	// Locate the 3 standard file userdata instances created during initIOLib().
	stdinUD := b.defaultInput.AsUserdata()
	stdoutUD := b.defaultOutput.AsUserdata()
	var stderrUD *Userdata
	if ioVal := b.env.RawGetStr(b.intern.Intern("io")); ioVal.IsTable() {
		if se := ioVal.AsTable().RawGetStr(b.intern.Intern("stderr")); se.IsUserdata() {
			stderrUD = se.AsUserdata()
		}
	}

	// BFS all reachable *Table pointers from bootstrap roots.
	tableIdx := make(map[*Table]int, maxStdTables)
	var tableList []*Table
	addTable := func(t *Table) int {
		if t == nil {
			return -1
		}
		if idx, ok := tableIdx[t]; ok {
			return idx
		}
		idx := len(tableList)
		tableIdx[t] = idx
		tableList = append(tableList, t)
		return idx
	}

	addTable(b.env)
	addTable(b.registry)
	addTable(b.stringMeta)
	addTable(b.fileMeta)
	addTable(b.pkgTbl)
	addTable(b.loadedTbl)
	addTable(b.preloadTbl)

	for i := 0; i < len(tableList); i++ {
		t := tableList[i]
		if t.meta != nil {
			addTable(t.meta)
		}
		for _, v := range t.slots {
			if v.IsTable() {
				addTable(v.AsTable())
			}
		}
		if t.arrKind == arrGeneric && t.arrLen > 0 {
			for _, v := range t.arrValSlice() {
				if v.IsTable() {
					addTable(v.AsTable())
				}
			}
		}
		if t.hash != nil {
			for k := range t.hash.nodes {
				nd := &t.hash.nodes[k]
				if nd.used && !nd.dead {
					if nd.key.IsTable() {
						addTable(nd.key.AsTable())
					}
					if nd.val.IsTable() {
						addTable(nd.val.AsTable())
					}
				}
			}
		}
	}

	if len(tableList) > maxStdTables {
		panic("moelua: stdlib table count exceeds maxStdTables")
	}

	tmpl := &stdTemplate{
		short:             b.intern.SnapshotFrozen(),
		tms:               b.intern.tms,
		nextStrID:         b.intern.nextID,
		nextShapeID:       b.shapes.nextID,
		builtinNext:         b.builtinNext,
		builtinIPairsIter:   b.builtinIPairsIter,
		builtinDebugGetInfo: b.builtinDebugGetInfo,
		builtinCoroYield:    b.builtinCoroYield,
		tables:            make([]tmplTableDesc, len(tableList)),
		envIdx:            tableIdx[b.env],
		registryIdx:       tableIdx[b.registry],
		stringMetaIdx:     tableIdx[b.stringMeta],
		fileMetaIdx:       tableIdx[b.fileMeta],
		pkgIdx:            tableIdx[b.pkgTbl],
		loadedIdx:         tableIdx[b.loadedTbl],
		preloadIdx:        tableIdx[b.preloadTbl],
	}
	fillNil(tmpl.nilStack[:])

	classifyReloc := func(v Value) (tmplRelocKind, int, bool) {
		switch {
		case v.IsTable():
			if idx, ok := tableIdx[v.AsTable()]; ok {
				return relocTable, idx, true
			}
		case v.IsThread():
			if v.AsThread() == b.mainThread {
				return relocMainThread, 0, true
			}
		case v.IsUserdata():
			u := v.AsUserdata()
			switch u {
			case stdinUD:
				return relocStdIn, 0, true
			case stdoutUD:
				return relocStdOut, 0, true
			case stderrUD:
				return relocStdErr, 0, true
			}
		}
		return 0, 0, false
	}

	for i, t := range tableList {
		if t.shape != nil {
			if t.shape.meta != nil {
				panic("moelua: template shape must not capture bootstrap metatable")
			}
			freezeShapeChain(t.shape)
		}
		if t.arrKind != arrEmpty && t.arrKind != arrGeneric {
			panic("moelua: template table must not use specialized primitive array")
		}
		desc := tmplTableDesc{
			base: Table{
				arrKind: t.arrKind,
				tmFlags: t.tmFlags,
				flags:   t.flags,
				arrHint: t.arrHint,
				shape:   t.shape,
			},
			metaIdx: -1,
		}
		if t.hash != nil {
			desc.hashBase = tableHash{
				mask:    t.hash.mask,
				count:   t.hash.count,
				live:    t.hash.live,
				dictGen: t.hash.dictGen,
			}
		}
		if t.meta != nil {
			desc.metaIdx = tableIdx[t.meta]
		}
		if len(t.slots) > 0 {
			desc.slotOff = len(tmpl.slots)
			desc.slotLen = len(t.slots)
			for j, v := range t.slots {
				tmpl.slots = append(tmpl.slots, v)
				if kind, target, ok := classifyReloc(v); ok {
					tmpl.relocs = append(tmpl.relocs, tmplReloc{dest: destSlot, idx: desc.slotOff + j, kind: kind, target: target})
				}
			}
		}
		if t.hash != nil && len(t.hash.nodes) > 0 {
			nodes := t.hash.nodes
			desc.nodeOff = len(tmpl.nodes)
			desc.nodeLen = len(nodes)
			for j := range nodes {
				nd := nodes[j]
				tmpl.nodes = append(tmpl.nodes, nd)
				if nd.used && !nd.dead {
					if kind, target, ok := classifyReloc(nd.key); ok {
						tmpl.relocs = append(tmpl.relocs, tmplReloc{dest: destNodeKey, idx: desc.nodeOff + j, kind: kind, target: target})
					}
					if kind, target, ok := classifyReloc(nd.val); ok {
						tmpl.relocs = append(tmpl.relocs, tmplReloc{dest: destNodeVal, idx: desc.nodeOff + j, kind: kind, target: target})
					}
				}
			}
		}
		if t.arrKind == arrGeneric && t.arrLen > 0 {
			arr := t.arrValSlice()
			desc.arrOff = len(tmpl.arr)
			desc.arrLen = len(arr)
			for j, v := range arr {
				tmpl.arr = append(tmpl.arr, v)
				if kind, target, ok := classifyReloc(v); ok {
					tmpl.relocs = append(tmpl.relocs, tmplReloc{dest: destArr, idx: desc.arrOff + j, kind: kind, target: target})
				}
			}
		}
		tmpl.tables[i] = desc
	}

	return tmpl
}

// stateAlloc packs the per-State fixed-size structures into ONE single Go heap allocation.
type stateAlloc struct {
	state      State
	mainThread Thread
	initStack  [initFastStackCap]Value
	initFrames [initFastFrameCap]callFrame
	tables     [maxStdTables]Table
	hashes     [maxStdTables]tableHash
	stdFiles   [3]luaFile
	stdUDs     [3]Userdata
}

func cloneFromStdTemplate(noEnv bool) *State {
	tmpl := getStdTemplate()
	a := new(stateAlloc)

	s := &a.state
	s.intern = stringInterner{
		seed:   globalStrSeed,
		nextID: tmpl.nextStrID,
		frozen: tmpl.short,
		tms:    tmpl.tms,
	}
	s.shapes.init(tmpl.nextShapeID)
	s.out = os.Stdout
	s.errOut = os.Stderr
	s.startTime = time.Now()
	s.gc = newGCState()
	s.compilerFn = defaultCompileFunc
	s.compilerReaderFn = defaultCompileReaderFunc
	s.builtinNext = tmpl.builtinNext
	s.builtinIPairsIter = tmpl.builtinIPairsIter
	s.builtinDebugGetInfo = tmpl.builtinDebugGetInfo
	s.builtinCoroYield = tmpl.builtinCoroYield
	s.seedRNG(makeRandomSeed(s), 0)

	// Wire mainThread with embedded stack & frame slices (zero separate heap allocs!).
	a.initStack = tmpl.nilStack
	th := &a.mainThread
	th.state = s
	th.stack = a.initStack[:]
	th.frames = a.initFrames[:]
	th.status = ThreadOK
	th.errObj = NilValue()
	th.hook = NilValue()
	th.nny = 1
	s.mainThread = th
	s.cur = th

	// Bulk-copy flat table storage slabs (3 allocations total for all ~20 stdlib tables!).
	var slotsSlab []Value
	if len(tmpl.slots) > 0 {
		slotsSlab = make([]Value, len(tmpl.slots))
		copy(slotsSlab, tmpl.slots)
	}
	var nodesSlab []hashNode
	if len(tmpl.nodes) > 0 {
		nodesSlab = make([]hashNode, len(tmpl.nodes))
		copy(nodesSlab, tmpl.nodes)
	}
	var arrSlab []Value
	if len(tmpl.arr) > 0 {
		arrSlab = make([]Value, len(tmpl.arr))
		copy(arrSlab, tmpl.arr)
	}

	// Instantiate standard file handles (stdin, stdout, stderr).
	fileMeta := &a.tables[tmpl.fileMetaIdx]
	a.stdFiles[0] = luaFile{f: os.Stdin, isStd: true}
	a.stdFiles[1] = luaFile{f: os.Stdout, isStd: true}
	a.stdFiles[2] = luaFile{f: os.Stderr, isStd: true}
	a.stdUDs[0] = Userdata{Data: &a.stdFiles[0], Meta: fileMeta}
	a.stdUDs[1] = Userdata{Data: &a.stdFiles[1], Meta: fileMeta}
	a.stdUDs[2] = Userdata{Data: &a.stdFiles[2], Meta: fileMeta}
	stdinVal := UserdataValue(&a.stdUDs[0])
	stdoutVal := UserdataValue(&a.stdUDs[1])
	stderrVal := UserdataValue(&a.stdUDs[2])
	mainThreadVal := ThreadValue(th)

	// Apply internal relocations on the cloned slabs BEFORE slicing into Table headers.
	for _, r := range tmpl.relocs {
		var val Value
		switch r.kind {
		case relocTable:
			val = TableValue(&a.tables[r.target])
		case relocMainThread:
			val = mainThreadVal
		case relocStdIn:
			val = stdinVal
		case relocStdOut:
			val = stdoutVal
		case relocStdErr:
			val = stderrVal
		}
		switch r.dest {
		case destSlot:
			slotsSlab[r.idx] = val
		case destNodeKey:
			nodesSlab[r.idx].key = val
		case destNodeVal:
			nodesSlab[r.idx].val = val
		case destArr:
			arrSlab[r.idx] = val
		}
	}

	// Populate each Table header with 3-index capped sub-slices so later appends never bleed across tables!
	for i := range tmpl.tables {
		d := &tmpl.tables[i]
		t := &a.tables[i]
		*t = d.base
		if d.metaIdx >= 0 {
			t.meta = &a.tables[d.metaIdx]
		}
		if d.slotLen > 0 {
			if d.slotLen <= len(t.inlineSlots) {
				copy(t.inlineSlots[:d.slotLen], slotsSlab[d.slotOff:d.slotOff+d.slotLen])
				t.slots = t.inlineSlots[:d.slotLen]
			} else {
				t.slots = slotsSlab[d.slotOff : d.slotOff+d.slotLen : d.slotOff+d.slotLen]
			}
		} else if t.shape != nil {
			t.slots = t.inlineSlots[:0]
		}
		if d.nodeLen > 0 {
			h := &a.hashes[i]
			*h = d.hashBase
			h.nodes = nodesSlab[d.nodeOff : d.nodeOff+d.nodeLen : d.nodeOff+d.nodeLen]
			t.hash = h
		}
		if d.arrLen > 0 {
			t.setArrVal(arrSlab[d.arrOff : d.arrOff+d.arrLen : d.arrOff+d.arrLen])
		}
	}

	s.env = &a.tables[tmpl.envIdx]
	s.registry = &a.tables[tmpl.registryIdx]
	s.stringMeta = &a.tables[tmpl.stringMetaIdx]
	s.fileMeta = fileMeta
	s.pkgTbl = &a.tables[tmpl.pkgIdx]
	s.loadedTbl = &a.tables[tmpl.loadedIdx]
	s.preloadTbl = &a.tables[tmpl.preloadIdx]
	s.coallocTables = a.tables[:len(tmpl.tables)]
	s.coallocHashes = a.hashes[:len(tmpl.tables)]
	s.typeMetas[4] = s.stringMeta
	s.stdinInput = stdinVal
	s.defaultInput = stdinVal
	s.defaultOutput = stdoutVal
	s.gc.tier1 = false

	if !noEnv {
		s.applyPackageEnv()
	} else {
		s.registry.RawSetStr(s, s.intern.Intern("LUA_NOENV"), TrueValue())
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
	if v, ok := os.LookupEnv("LUA_PATH_5_5"); ok {
		s.pkgTbl.RawSetStr(s, s.intern.Intern("path"), StringValue(s.intern.Intern(expandDefaultPath(v, defaultLuaPath))))
	} else if v, ok := os.LookupEnv("LUA_PATH"); ok {
		s.pkgTbl.RawSetStr(s, s.intern.Intern("path"), StringValue(s.intern.Intern(expandDefaultPath(v, defaultLuaPath))))
	}
	if v, ok := os.LookupEnv("LUA_CPATH_5_5"); ok {
		s.pkgTbl.RawSetStr(s, s.intern.Intern("cpath"), StringValue(s.intern.Intern(expandDefaultPath(v, defaultLuaCPath))))
	} else if v, ok := os.LookupEnv("LUA_CPATH"); ok {
		s.pkgTbl.RawSetStr(s, s.intern.Intern("cpath"), StringValue(s.intern.Intern(expandDefaultPath(v, defaultLuaCPath))))
	}
}

func expandDefaultPath(val, dflt string) string {
	// Per Lua 5.5 loadlib.c:setpath (main.lua:222-235): replace ONLY the first ';;'
	// with the default path, preserving any leading/trailing single ';' separators.
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
