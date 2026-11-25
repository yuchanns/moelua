package vm

import (
	"hash/maphash"
	"sync/atomic"
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

type stringInterner struct {
	seed   uint32
	nextID uint32
	frozen map[string]*LuaString // read-only shared stdlib short strings (never mutated after template init)
	short  map[string]*LuaString // lazily allocated per-State map for user short strings
	tms    [tmCount]*LuaString
}

func newStringInterner(_ uint32) stringInterner {
	si := stringInterner{
		seed:   globalStrSeed,
		nextID: 1,
		short:  make(map[string]*LuaString, 160),
	}
	for i := range tmCount {
		si.tms[i] = si.Intern(tmNames[i])
	}
	return si
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

// Intern returns a canonical *LuaString for short strings (<= 40 bytes) or allocates a long string
// with lazy hashing (matching PUC-Rio Lua 5.5).
func (si *stringInterner) Intern(s string) *LuaString {
	if len(s) <= maxShortStringLen {
		if si.frozen != nil {
			if ls, ok := si.frozen[s]; ok {
				return ls
			}
		}
		if si.short != nil {
			if ls, ok := si.short[s]; ok {
				return ls
			}
		} else {
			si.short = make(map[string]*LuaString, 32)
		}
		ls := &LuaString{
			S:     s,
			Hash:  hashString(s, si.seed),
			ID:    si.nextID,
			Short: true,
		}
		si.nextID++
		si.short[s] = ls
		return ls
	}
	return &LuaString{
		S:     s,
		Hash:  0, // lazily hashed on first table-key lookup
		Short: false,
	}
}
