package vm

import (
	"hash/maphash"
	"strings"
	"sync/atomic"
	"weak"
)

const maxShortStringLen = 40

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

// LuaString represents a Lua 5.5 string.
// Short strings (len <= 40) are interned per State so pointer equality == string equality
// and hash/ID lookups never touch string bytes.
type LuaString struct {
	S     string
	Hash  uint32
	ID    uint32
	Short bool
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

type stringInterner struct {
	seed      uint32
	nextID    uint32
	shortAdds int
	frozen    map[string]*LuaString              // read-only shared stdlib short strings (never mutated after template init)
	short     map[uint32][]weak.Pointer[LuaString] // hash-bucketed weak interner so unreachable short strings are reclaimed
	tms       [tmCount]*LuaString
}

func newStringInterner(_ uint32) stringInterner {
	si := stringInterner{
		seed:   globalStrSeed,
		nextID: 1,
		short:  make(map[uint32][]weak.Pointer[LuaString], 160),
	}
	for i := range tmCount {
		si.tms[i] = si.Intern(tmNames[i])
	}
	return si
}

// SnapshotFrozen returns a strong map of all currently live short strings (used once when freezing stdTemplate).
func (si *stringInterner) SnapshotFrozen() map[string]*LuaString {
	out := make(map[string]*LuaString, len(si.short))
	for _, bucket := range si.short {
		for _, wp := range bucket {
			if ls := wp.Value(); ls != nil {
				out[ls.S] = ls
			}
		}
	}
	return out
}

// Prune removes collected weak entries and empty hash buckets from the per-State interner.
func (si *stringInterner) Prune() int {
	si.shortAdds = 0
	if len(si.short) == 0 {
		return 0
	}
	live := 0
	for h, bucket := range si.short {
		kept := bucket[:0]
		for _, wp := range bucket {
			if wp.Value() != nil {
				kept = append(kept, wp)
			}
		}
		if len(kept) == 0 {
			delete(si.short, h)
		} else {
			clear(bucket[len(kept):])
			si.short[h] = kept
			live += len(kept)
		}
	}
	return live
}

// hash returns the cached hash, computing it lazily for long strings (len > 40)
// using the process-global randomized seed. Short strings have immutable precomputed
// hashes; long strings use atomic load/store so shared compiled protos never race.
func (ls *LuaString) hash() uint32 {
	if ls.Short {
		return ls.Hash
	}
	if h := atomic.LoadUint32(&ls.Hash); h != 0 {
		return h
	}
	h := hashString(ls.S, globalStrSeed)
	atomic.StoreUint32(&ls.Hash, h)
	return h
}

func (ls *LuaString) hashWithCancel(cancelFn func() error) (uint32, error) {
	if ls.Short {
		return ls.Hash, nil
	}
	if h := atomic.LoadUint32(&ls.Hash); h != 0 {
		return h, nil
	}
	h, err := hashStringWithCancel(ls.S, globalStrSeed, cancelFn)
	if err != nil {
		return 0, err
	}
	atomic.StoreUint32(&ls.Hash, h)
	return h, nil
}

func (s *State) ensureStrKeyHash(ls *LuaString) error {
	if s == nil || ls == nil || ls.Short || len(ls.S) <= strHashCancelChunk || atomic.LoadUint32(&ls.Hash) != 0 {
		return nil
	}
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil {
		return nil
	}
	_, err := ls.hashWithCancel(cancelFn)
	return err
}

func (s *State) ensureLookupStrKeyHash(t *Table, ls *LuaString) error {
	if t == nil || t.hash == nil || len(t.hash.nodes) == 0 {
		return nil
	}
	return s.ensureStrKeyHash(ls)
}

func (s *State) ensureLookupKeyHash(t *Table, key Value) error {
	if !key.IsString() {
		return nil
	}
	return s.ensureLookupStrKeyHash(t, key.AsString())
}

func (s *State) ensureWriteStrKeyHash(t *Table, ls *LuaString, valNil bool) error {
	if valNil && (t == nil || t.hash == nil || len(t.hash.nodes) == 0) {
		return nil
	}
	return s.ensureStrKeyHash(ls)
}

// lookupExistingShort returns an already-interned short *LuaString if present (without allocating).
func (si *stringInterner) lookupExistingShort(s string) (*LuaString, bool) {
	if len(s) > maxShortStringLen {
		return nil, false
	}
	if si.frozen != nil {
		if ls, ok := si.frozen[s]; ok {
			return ls, true
		}
	}
	if si.short != nil {
		h := hashString(s, si.seed)
		for _, wp := range si.short[h] {
			if ls := wp.Value(); ls != nil && ls.S == s {
				return ls, true
			}
		}
	}
	return nil, false
}

// Intern returns a canonical *LuaString for short strings (<= 40 bytes) or allocates a long string
// with lazy hashing (matching PUC-Rio Lua 5.5).
func (si *stringInterner) Intern(s string) *LuaString {
	if len(s) <= maxShortStringLen {
		if si.frozen != nil {
			if ls, ok := si.frozen[s]; ok {
				return ls
			}
		}
		h := hashString(s, si.seed)
		if si.short != nil {
			if bucket := si.short[h]; len(bucket) > 0 {
				var found *LuaString
				dead := false
				for _, wp := range bucket {
					ls := wp.Value()
					if ls == nil {
						dead = true
						continue
					}
					if ls.S == s {
						found = ls
						if !dead {
							return ls
						}
					}
				}
				if dead {
					kept := bucket[:0]
					for _, wp := range bucket {
						if wp.Value() != nil {
							kept = append(kept, wp)
						}
					}
					clear(bucket[len(kept):])
					if len(kept) == 0 {
						delete(si.short, h)
					} else {
						si.short[h] = kept
					}
				}
				if found != nil {
					return found
				}
			}
		} else {
			si.short = make(map[uint32][]weak.Pointer[LuaString], 32)
		}
		ls := &LuaString{
			S:     strings.Clone(s),
			Hash:  h,
			ID:    si.nextID,
			Short: true,
		}
		si.nextID++
		si.short[h] = append(si.short[h], weak.Make(ls))
		si.shortAdds++
		if si.shortAdds >= 1024 && len(si.short) > 64 {
			si.Prune()
		}
		return ls
	}
	return &LuaString{
		S:     s,
		Hash:  0, // lazily hashed on first table-key lookup
		Short: false,
	}
}
