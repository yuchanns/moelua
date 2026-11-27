package vm

import (
	"hash/maphash"
	"io"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

const maxShortStringLen = 40

const (
	strFlagFixed uint8 = 1 << 0 // frozen process-wide stdlib string (immutable, never swept)
)

// globalStrSeed is initialized once per process from crypto/random maphash.MakeSeed()
// so both short and lazy long strings resist HashDoS while remaining cross-State shareable.
var globalStrSeed = initGlobalStrSeed()

func initGlobalStrSeed() uint32 {
	var h maphash.Hash
	h.SetSeed(maphash.MakeSeed())
	h.WriteString("moelua55")
	s := uint32(h.Sum64())
	if s == 0 {
		s = 0x9e3779b9
	}
	return s
}

// LuaString is a noscan Lua 5.5 string stored contiguously inside an Arena slab page
// or dedicated large span: 32-byte header followed immediately by Len inline payload bytes.
// ptrdata(LuaString) == 0 so Go's GC never scans Lua strings.
// Short strings (Len <= 40) are interned per State via intrusive HNext chains so ObjRef
// equality == string equality and hash/ID lookups never touch string bytes.
type LuaString struct {
	hdr   arena.GCHeader // 8B: Next (allgc list), Kind (short/long), Mark, Class, Flags
	ID    uint64         // 8B: monotonically unique per-State short-string ID (0 for long strings)
	Hash  uint32         // 4B: FNV-1a hash (eager for short strings, lazy for long strings)
	Len   uint32         // 4B: exact byte length
	HNext arena.ObjRef   // 4B: intrusive hash bucket chain in stringInterner.buckets (short only)
	self  arena.ObjRef   // 4B: own ObjRef inside frozenArena (if strFlagFixed) or State.arena
}

const luaStringHeaderSize = int(unsafe.Sizeof(LuaString{}))

const (
	_ = uint(32 - unsafe.Sizeof(LuaString{}))
	_ = uint(unsafe.Sizeof(LuaString{}) - 32)
)

// Str returns a zero-copy string view of ls's inline Arena payload bytes.
// Valid while ls remains live in its owning State's Arena; host callers retaining
// a Go string beyond Lua GC or State.Close must copy via strings.Clone.
func (ls *LuaString) Str() string {
	if ls == nil || ls.Len == 0 {
		return ""
	}
	return unsafe.String((*byte)(unsafe.Add(unsafe.Pointer(ls), luaStringHeaderSize)), ls.Len)
}

// IsShort reports whether ls is an interned short string (Len <= 40).
func (ls *LuaString) IsShort() bool {
	return ls != nil && ls.hdr.Kind == gcKindShortStr
}

// IsFixed reports whether ls belongs to the immutable process-wide frozen stdlib arena.
func (ls *LuaString) IsFixed() bool {
	return ls != nil && ls.hdr.Flags&strFlagFixed != 0
}

// newTransientLongString wraps str in a standalone noscan byte buffer for non-mutating
// read-only table lookups (State.LookupString) without touching State.arena.
func newTransientLongString(str string) *LuaString {
	buf := make([]byte, luaStringHeaderSize+len(str))
	ls := (*LuaString)(unsafe.Pointer(unsafe.SliceData(buf)))
	ls.hdr.Kind = gcKindLongStr
	ls.hdr.Flags = strFlagFixed
	ls.Len = uint32(len(str))
	copy(buf[luaStringHeaderSize:], str)
	return ls
}

// Metamethod tag indices matching PUC-Rio Lua 5.5.1 ltm.h.
type TM uint8

const (
	TMIndex TM = iota
	TMNewIndex
	TMGC
	TMMode
	TMLen
	TMEq
	TMAdd
	TMSub
	TMMul
	TMMod
	TMPow
	TMDiv
	TMIDiv
	TMBand
	TMBor
	TMBxor
	TMShl
	TMShr
	TMUnm
	TMBnot
	TMLt
	TMLe
	TMConcat
	TMCall
	TMClose
	TMToString
	TMPairs
	tmCount
)

var tmNames = [tmCount]string{
	TMIndex:    "__index",
	TMNewIndex: "__newindex",
	TMGC:       "__gc",
	TMMode:     "__mode",
	TMLen:      "__len",
	TMEq:       "__eq",
	TMAdd:      "__add",
	TMSub:      "__sub",
	TMMul:      "__mul",
	TMMod:      "__mod",
	TMPow:      "__pow",
	TMDiv:      "__div",
	TMIDiv:     "__idiv",
	TMBand:     "__band",
	TMBor:      "__bor",
	TMBxor:     "__bxor",
	TMShl:      "__shl",
	TMShr:      "__shr",
	TMUnm:      "__unm",
	TMBnot:     "__bnot",
	TMLt:       "__lt",
	TMLe:       "__le",
	TMConcat:   "__concat",
	TMCall:     "__call",
	TMClose:    "__close",
	TMToString: "__tostring",
	TMPairs:    "__pairs",
}

const strHashCancelChunk = 4096
const strCmpCancelChunk = 65536
const strCloneCancelChunk = 65536

func indexByteWithCancel(src string, c byte, cancelFn func() error) (int, error) {
	if cancelFn == nil || len(src) <= strCloneCancelChunk {
		return strings.IndexByte(src, c), nil
	}
	for off := 0; off < len(src); off += strCloneCancelChunk {
		if off > 0 {
			if err := cancelFn(); err != nil {
				return -1, err
			}
		}
		end := min(off+strCloneCancelChunk, len(src))
		if idx := strings.IndexByte(src[off:end], c); idx >= 0 {
			return off + idx, nil
		}
	}
	return -1, nil
}

func containsByteWithCancel(src string, c byte, cancelFn func() error) (bool, error) {
	idx, err := indexByteWithCancel(src, c, cancelFn)
	return idx >= 0, err
}

// writeStringWithCancel appends src to sb in bounded 64 KiB chunks while polling cancelFn
// (zero overhead when cancelFn == nil, and zero extra charge on short strings below 64 KiB).
func writeStringWithCancel(sb *strings.Builder, src string, cancelFn func() error, work *int) error {
	if cancelFn == nil || len(src) == 0 {
		sb.WriteString(src)
		return nil
	}
	for off := 0; off < len(src); {
		rem := strCloneCancelChunk - *work
		if rem <= 0 {
			if err := cancelFn(); err != nil {
				return err
			}
			*work = 0
			rem = strCloneCancelChunk
		}
		end := min(off+rem, len(src))
		sb.WriteString(src[off:end])
		*work += end - off
		off = end
		if *work >= strCloneCancelChunk {
			if err := cancelFn(); err != nil {
				return err
			}
			*work = 0
		}
	}
	return nil
}

// joinStringsWithCancel concatenates parts while checking memLimit (when enabled)
// and polling execution controls every 64 KiB.
func (s *State) joinStringsWithCancel(parts ...string) (string, error) {
	var total int64
	for _, p := range parts {
		total += int64(len(p))
	}
	if s.memLimit > 0 {
		if err := s.checkMem(total + 48); err != nil {
			return "", err
		}
	}
	cancelFn := s.nativeCancelFunc()
	var sb strings.Builder
	if cancelFn == nil {
		sb.Grow(int(total))
		for _, p := range parts {
			sb.WriteString(p)
		}
		return sb.String(), nil
	}
	work := 0
	for _, p := range parts {
		if err := writeStringWithCancel(&sb, p, cancelFn, &work); err != nil {
			return "", err
		}
	}
	return sb.String(), nil
}

// writeIOStringWithCancel writes src to w in bounded 64 KiB chunks while polling cancelFn.
func writeIOStringWithCancel(w io.Writer, src string, cancelFn func() error, work *int) error {
	if cancelFn == nil || len(src) == 0 {
		_, _ = io.WriteString(w, src)
		return nil
	}
	for off := 0; off < len(src); {
		rem := strCloneCancelChunk - *work
		if rem <= 0 {
			if err := cancelFn(); err != nil {
				return err
			}
			*work = 0
			rem = strCloneCancelChunk
		}
		end := min(off+rem, len(src))
		if _, err := io.WriteString(w, src[off:end]); err != nil {
			return nil
		}
		*work += end - off
		off = end
		if *work >= strCloneCancelChunk {
			if err := cancelFn(); err != nil {
				return err
			}
			*work = 0
		}
	}
	return nil
}

// countStringWithCancel counts non-overlapping occurrences of sep in src, polling cancelFn
// every 64 KiB when scanning large strings.
func countStringWithCancel(src, sep string, cancelFn func() error) (int64, error) {
	if cancelFn == nil || sep == "" || len(src) <= strCloneCancelChunk || len(sep) > len(src) {
		return int64(strings.Count(src, sep)), nil
	}
	var cnt int64
	rem := src
	scanned := 0
	for len(rem) >= len(sep) {
		win := min(len(rem), strCloneCancelChunk+len(sep)-1)
		idx := strings.Index(rem[:win], sep)
		if idx < 0 {
			if win == len(rem) {
				break
			}
			rem = rem[strCloneCancelChunk:]
			if err := cancelFn(); err != nil {
				return 0, err
			}
			scanned = 0
			continue
		}
		cnt++
		adv := idx + len(sep)
		rem = rem[adv:]
		scanned += adv
		if scanned >= strCloneCancelChunk {
			if err := cancelFn(); err != nil {
				return 0, err
			}
			scanned = 0
		}
	}
	return cnt, nil
}

// replaceAllWithCancel behaves like strings.ReplaceAll while polling cancelFn every 64 KiB.
func replaceAllWithCancel(src, oldStr, newStr string, cancelFn func() error) (string, error) {
	if cancelFn == nil || oldStr == "" || len(oldStr) > len(src) ||
		(len(src) <= strCloneCancelChunk && len(newStr) <= strCloneCancelChunk) {
		return strings.ReplaceAll(src, oldStr, newStr), nil
	}
	var sb strings.Builder
	work := 0
	rem := src
	for len(rem) > 0 {
		if len(rem) < len(oldStr) {
			if err := writeStringWithCancel(&sb, rem, cancelFn, &work); err != nil {
				return "", err
			}
			break
		}
		win := min(len(rem), strCloneCancelChunk+len(oldStr)-1)
		idx := strings.Index(rem[:win], oldStr)
		if idx < 0 {
			if win == len(rem) {
				if err := writeStringWithCancel(&sb, rem, cancelFn, &work); err != nil {
					return "", err
				}
				break
			}
			if err := writeStringWithCancel(&sb, rem[:strCloneCancelChunk], cancelFn, &work); err != nil {
				return "", err
			}
			rem = rem[strCloneCancelChunk:]
			continue
		}
		if idx > 0 {
			if err := writeStringWithCancel(&sb, rem[:idx], cancelFn, &work); err != nil {
				return "", err
			}
		}
		if err := writeStringWithCancel(&sb, newStr, cancelFn, &work); err != nil {
			return "", err
		}
		rem = rem[idx+len(oldStr):]
	}
	return sb.String(), nil
}

// cloneStringWithCancel clones src while polling execution controls every 64 KiB
// when a context deadline or instruction budget is active.
func (s *State) cloneStringWithCancel(src string) (string, error) {
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil || len(src) <= strCloneCancelChunk {
		return strings.Clone(src), nil
	}
	var sb strings.Builder
	for off := 0; off < len(src); off += strCloneCancelChunk {
		if off > 0 {
			if err := cancelFn(); err != nil {
				return "", err
			}
		}
		end := min(off+strCloneCancelChunk, len(src))
		sb.WriteString(src[off:end])
	}
	return sb.String(), nil
}

// compareStrings compares two Lua strings lexicographically (matching Lua's l_strcmp),
// polling State execution controls every 64 KiB for long strings with shared prefixes.
func (s *State) compareStrings(a, b *LuaString) (int, error) {
	if a == b {
		return 0, nil
	}
	sa, sb := a.Str(), b.Str()
	n := min(len(sa), len(sb))
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil || n <= strCmpCancelChunk {
		if sa < sb {
			return -1, nil
		}
		if sa > sb {
			return 1, nil
		}
		return 0, nil
	}
	for off := 0; off < n; off += strCmpCancelChunk {
		if off > 0 {
			if err := cancelFn(); err != nil {
				return 0, err
			}
		}
		end := min(off+strCmpCancelChunk, n)
		ca, cb := sa[off:end], sb[off:end]
		if ca != cb {
			if ca < cb {
				return -1, nil
			}
			return 1, nil
		}
	}
	if len(sa) < len(sb) {
		return -1, nil
	}
	if len(sa) > len(sb) {
		return 1, nil
	}
	return 0, nil
}

// stringsEqual compares two Lua strings for equality, polling State execution controls
// every 64 KiB when comparing distinct long strings of identical length.
func (s *State) stringsEqual(a, b *LuaString) (bool, error) {
	if a == b {
		return true, nil
	}
	if a.IsShort() || b.IsShort() {
		return false, nil
	}
	if a.Len != b.Len {
		return false, nil
	}
	if ha, hb := atomic.LoadUint32(&a.Hash), atomic.LoadUint32(&b.Hash); ha != 0 && hb != 0 && ha != hb {
		return false, nil
	}
	sa, sb := a.Str(), b.Str()
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil || len(sa) <= strCmpCancelChunk {
		return sa == sb, nil
	}
	for off := 0; off < len(sa); off += strCmpCancelChunk {
		if off > 0 {
			if err := cancelFn(); err != nil {
				return false, err
			}
		}
		end := min(off+strCmpCancelChunk, len(sa))
		if sa[off:end] != sb[off:end] {
			return false, nil
		}
	}
	return true, nil
}

// rawEqual implements Lua 5.5 rawequal(a, b) with cooperative cancel polling on long strings.
func (s *State) rawEqual(a, b Value) (bool, error) {
	if a.IsString() && b.IsString() {
		if a == b {
			return true, nil
		}
		return s.stringsEqual(s.asString(a), s.asString(b))
	}
	return RawEqual(a, b), nil
}

// hashString computes a fast 32-bit FNV-1a hash seeded with state seed.
func hashString(s string, seed uint32) uint32 {
	h := uint32(2166136261) ^ seed
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	if h == 0 {
		h = 1
	}
	return h
}

// hashStringWithCancel computes the same 32-bit FNV-1a hash as hashString, polling cancelFn
// every strHashCancelChunk bytes so long string keys respect context deadlines and instruction limits.
func hashStringWithCancel(s string, seed uint32, cancelFn func() error) (uint32, error) {
	if cancelFn == nil || len(s) <= strHashCancelChunk {
		return hashString(s, seed), nil
	}
	h := uint32(2166136261) ^ seed
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
		if (i & (strHashCancelChunk - 1)) == (strHashCancelChunk - 1) {
			if err := cancelFn(); err != nil {
				return 0, err
			}
		}
	}
	if h == 0 {
		h = 1
	}
	return h, nil
}

const (
	minStrBuckets = 32
)

type stringInterner struct {
	seed   uint32
	nextID uint64

	// Immutable process-wide frozen stdlib short strings (shared read-only across States).
	frozenArena   *arena.Arena
	frozenBuckets []arena.ObjRef
	frozenMask    uint32

	// Per-State unified noscan Arena + Collector and PUC-Rio lstring.c short-string hash table.
	// During bootstrap (bootstrapping == true), strings are allocated into bootstrapArena and
	// frozen into stdTemplate.strArena so cloned States share all stdlib strings read-only.
	arena         *arena.Arena
	collector     *arena.Collector
	bootstrapping bool
	bootArena     *arena.Arena
	bootAll       arena.ObjRef
	buckets       []arena.ObjRef // noscan []uint32; power-of-2 length
	nuse          int32          // count of live dynamic short strings in buckets

	tms [tmCount]*LuaString
}

var fixedErrMsgs = [...]string{
	"not enough memory",
	"C stack overflow",
	"<no error object>",
}

func newBootstrapStringInterner() stringInterner {
	boot := arena.New()
	si := stringInterner{
		seed:          globalStrSeed,
		nextID:        1,
		frozenArena:   boot,
		bootstrapping: true,
		bootArena:     boot,
	}
	for i := range tmCount {
		si.tms[i] = si.Intern(tmNames[i])
	}
	for _, msg := range fixedErrMsgs {
		si.Intern(msg)
	}
	return si
}

func (si *stringInterner) activeArena() *arena.Arena {
	if si.bootstrapping {
		return si.bootArena
	}
	return si.arena
}

func (si *stringInterner) resizeBuckets(newSize int) {
	if newSize < minStrBuckets {
		newSize = minStrBuckets
	}
	nb := make([]arena.ObjRef, newSize)
	mask := uint32(newSize - 1)
	a := si.activeArena()
	for _, head := range si.buckets {
		for ref := head; ref != arena.NullRef; {
			ls := arena.AsSmall[LuaString](a, ref)
			next := ls.HNext
			idx := ls.Hash & mask
			ls.HNext = nb[idx]
			nb[idx] = ref
			ref = next
		}
	}
	si.buckets = nb
}

// FreezeBootstrap marks all strings allocated during bootstrap as immutable fixed
// stdlib strings and returns the read-only frozen arena + bucket table for stdTemplate.
func (si *stringInterner) FreezeBootstrap() (*arena.Arena, []arena.ObjRef, uint32) {
	if si.bootArena == nil {
		return nil, nil, 0
	}
	for ref := si.bootAll; ref != arena.NullRef; {
		ls := arena.AsFast[LuaString](si.bootArena, ref)
		ls.hdr.Flags |= strFlagFixed
		ref = ls.hdr.Next
	}
	if len(si.buckets) == 0 {
		si.buckets = make([]arena.ObjRef, minStrBuckets)
	}
	want := minStrBuckets
	for want < int(si.nuse)*2 {
		want <<= 1
	}
	if want > len(si.buckets) {
		si.resizeBuckets(want)
	}
	return si.bootArena, si.buckets, uint32(len(si.buckets) - 1)
}

func (si *stringInterner) unlinkBucket(target arena.ObjRef, hash uint32) {
	if len(si.buckets) == 0 || si.arena == nil {
		return
	}
	idx := hash & uint32(len(si.buckets)-1)
	a := si.arena
	var prev arena.ObjRef
	for cur := si.buckets[idx]; cur != arena.NullRef; {
		ls := arena.AsSmall[LuaString](a, cur)
		if cur == target {
			if prev == arena.NullRef {
				si.buckets[idx] = ls.HNext
			} else {
				arena.AsSmall[LuaString](a, prev).HNext = ls.HNext
			}
			ls.HNext = arena.NullRef
			si.nuse--
			return
		}
		prev = cur
		cur = ls.HNext
	}
}

// checkSizes shrinks the dynamic short-string hash table when sparsely populated
// (matching PUC-Rio lgc.c:checkSizes).
func (si *stringInterner) checkSizes() {
	if len(si.buckets) > minStrBuckets && int(si.nuse) < len(si.buckets)/4 {
		si.resizeBuckets(len(si.buckets) / 2)
	}
}

// LiveDynamicShort returns the number of live dynamic short strings in this State.
func (si *stringInterner) LiveDynamicShort() int {
	return int(si.nuse)
}

// Close releases per-State dynamic string table metadata (State.Close frees the Arena).
func (si *stringInterner) Close() {
	si.buckets = nil
	si.nuse = 0
	si.arena = nil
	si.collector = nil
}

// hash returns the cached hash, computing it lazily for long strings (len > 40)
// using the process-global randomized seed. Short strings have immutable precomputed
// hashes; long strings use atomic load/store so concurrent reads never race.
func (ls *LuaString) hash() uint32 {
	if ls.IsShort() {
		return ls.Hash
	}
	if h := atomic.LoadUint32(&ls.Hash); h != 0 {
		return h
	}
	h := hashString(ls.Str(), globalStrSeed)
	atomic.StoreUint32(&ls.Hash, h)
	return h
}

func (ls *LuaString) hashWithCancel(cancelFn func() error) (uint32, error) {
	if ls.IsShort() {
		return ls.Hash, nil
	}
	if h := atomic.LoadUint32(&ls.Hash); h != 0 {
		return h, nil
	}
	h, err := hashStringWithCancel(ls.Str(), globalStrSeed, cancelFn)
	if err != nil {
		return 0, err
	}
	atomic.StoreUint32(&ls.Hash, h)
	return h, nil
}

func (s *State) ensureStrKeyHash(ls *LuaString) error {
	if s == nil || ls == nil || ls.IsShort() || int(ls.Len) <= strHashCancelChunk || atomic.LoadUint32(&ls.Hash) != 0 {
		return nil
	}
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil {
		return nil
	}
	_, err := ls.hashWithCancel(cancelFn)
	return err
}

// checkTableLongStrProbe cooperatively polls execution controls before table hash probes
// compare a huge long string against distinct same-length long strings in the probe chain.
func (s *State) checkTableLongStrProbe(t *Table, ls *LuaString) error {
	if s == nil || t == nil || t.hashRef == arena.NullRef || ls == nil || ls.IsShort() || int(ls.Len) <= strCmpCancelChunk || !s.hasInterruptControl() {
		return nil
	}
	h := arena.AsSmall[tableHash](s.arena, t.hashRef)
	if h.nodesRef == arena.NullRef {
		return nil
	}
	nodes := h.nodesSlice(s.arena)
	mask := h.mask
	idx := ls.hash() & mask
	for {
		nd := &nodes[idx]
		if nd.state == nodeEmpty {
			return nil
		}
		if nd.key.IsString() {
			other := s.asString(nd.key)
			if other != ls && !other.IsShort() && other.Len == ls.Len {
				eq, err := s.stringsEqual(other, ls)
				if err != nil {
					return err
				}
				if eq && nd.state == nodeLive {
					return nil
				}
			}
		}
		idx = (idx + 1) & mask
	}
}

func (s *State) ensureLookupStrKeyHash(t *Table, ls *LuaString) error {
	if t == nil || t.hashRef == arena.NullRef {
		return nil
	}
	if err := s.ensureStrKeyHash(ls); err != nil {
		return err
	}
	return s.checkTableLongStrProbe(t, ls)
}

func (s *State) ensureLookupKeyHash(t *Table, key Value) error {
	if !key.IsString() {
		return nil
	}
	return s.ensureLookupStrKeyHash(t, s.asString(key))
}

func (s *State) ensureWriteStrKeyHash(t *Table, ls *LuaString, valNil bool) error {
	if valNil && (t == nil || t.hashRef == arena.NullRef) {
		return nil
	}
	if err := s.ensureStrKeyHash(ls); err != nil {
		return err
	}
	return s.checkTableLongStrProbe(t, ls)
}

// lookupExistingShort returns an already-interned short *LuaString if present (without allocating).
func (si *stringInterner) lookupExistingShort(s string) (*LuaString, bool) {
	if len(s) > maxShortStringLen {
		return nil, false
	}
	h := hashString(s, si.seed)
	if len(si.frozenBuckets) > 0 {
		for ref := si.frozenBuckets[h&si.frozenMask]; ref != arena.NullRef; {
			ls := arena.AsSmall[LuaString](si.frozenArena, ref)
			if ls.Hash == h && ls.Len == uint32(len(s)) && ls.Str() == s {
				return ls, true
			}
			ref = ls.HNext
		}
	}
	a := si.activeArena()
	if len(si.buckets) > 0 && a != nil {
		mask := uint32(len(si.buckets) - 1)
		for ref := si.buckets[h&mask]; ref != arena.NullRef; {
			ls := arena.AsSmall[LuaString](a, ref)
			if ls.Hash == h && ls.Len == uint32(len(s)) && ls.Str() == s {
				if !si.bootstrapping && si.collector != nil && ls.hdr.IsDead(si.collector.OtherWhite()) {
					// Resurrect dead-but-unswept short string during incremental sweep (lstring.c:internshrstr).
					ls.hdr.SetWhite(si.collector.CurrentWhite())
					ls.hdr.SetAge(arena.AgeNew)
				}
				return ls, true
			}
			ref = ls.HNext
		}
	}
	return nil, false
}

// Intern returns a canonical *LuaString for short strings (<= 40 bytes) or allocates a long string
// with lazy hashing inside the per-State noscan Arena (managed by State.collector).
func (si *stringInterner) Intern(s string) *LuaString {
	if len(s) <= maxShortStringLen {
		h := hashString(s, si.seed)
		if len(si.frozenBuckets) > 0 {
			for ref := si.frozenBuckets[h&si.frozenMask]; ref != arena.NullRef; {
				ls := arena.AsSmall[LuaString](si.frozenArena, ref)
				if ls.Hash == h && ls.Len == uint32(len(s)) && ls.Str() == s {
					return ls
				}
				ref = ls.HNext
			}
		}
		a := si.activeArena()
		if len(si.buckets) > 0 && a != nil {
			mask := uint32(len(si.buckets) - 1)
			for ref := si.buckets[h&mask]; ref != arena.NullRef; {
				ls := arena.AsSmall[LuaString](a, ref)
				if ls.Hash == h && ls.Len == uint32(len(s)) && ls.Str() == s {
					if !si.bootstrapping && si.collector != nil && ls.hdr.IsDead(si.collector.OtherWhite()) {
						// Resurrect dead-but-unswept short string during incremental sweep (lstring.c:internshrstr).
						ls.hdr.SetWhite(si.collector.CurrentWhite())
						ls.hdr.SetAge(arena.AgeNew)
					}
					return ls
				}
				ref = ls.HNext
			}
		}
		total := luaStringHeaderSize + len(s)
		var (
			ref arena.ObjRef
			err error
		)
		if si.bootstrapping {
			ref, err = si.bootArena.AllocGC(total, gcKindShortStr, arena.MarkWhite0)
		} else {
			ref, err = si.collector.Alloc(total, gcKindShortStr)
		}
		if err != nil {
			panic(err)
		}
		ls := arena.AsSmall[LuaString](a, ref)
		ls.ID = si.nextID
		si.nextID++
		ls.Hash = h
		ls.Len = uint32(len(s))
		ls.self = ref
		if si.bootstrapping {
			ls.hdr.Flags |= strFlagFixed
			ls.hdr.Next = si.bootAll
			si.bootAll = ref
		}
		if len(s) > 0 {
			copy(a.Bytes(ref, total)[luaStringHeaderSize:], s)
		}
		if len(si.buckets) == 0 {
			si.buckets = make([]arena.ObjRef, minStrBuckets)
		} else if int(si.nuse) >= len(si.buckets) {
			si.resizeBuckets(len(si.buckets) * 2)
		}
		idx := h & uint32(len(si.buckets)-1)
		ls.HNext = si.buckets[idx]
		si.buckets[idx] = ref
		si.nuse++
		return ls
	}
	total := luaStringHeaderSize + len(s)
	a := si.activeArena()
	var (
		ref arena.ObjRef
		err error
	)
	if si.bootstrapping {
		ref, err = si.bootArena.AllocGC(total, gcKindLongStr, arena.MarkWhite0)
	} else {
		ref, err = si.collector.Alloc(total, gcKindLongStr)
	}
	if err != nil {
		return newTransientLongString(s)
	}
	ls := arena.AsFast[LuaString](a, ref)
	ls.Len = uint32(len(s))
	ls.self = ref
	if si.bootstrapping {
		ls.hdr.Flags |= strFlagFixed
		ls.hdr.Next = si.bootAll
		si.bootAll = ref
	}
	if len(s) > 0 {
		copy(a.Bytes(ref, total)[luaStringHeaderSize:], s)
	}
	return ls
}
