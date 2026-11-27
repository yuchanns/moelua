//go:build (arm64 || amd64) && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"runtime"
	"slices"
	"sync"
	"weak"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/jit/ir"
)

const (
	// maxStructuralCacheEntries caps the number of distinct compiled Tier-1 protos kept
	// in the structural-fingerprint LRU (deduplicating repeated DoString parses across VMs).
	maxStructuralCacheEntries = 64
	// maxStructuralProtoCodeLen skips structural-LRU snapshot storage for huge chunks
	// (those still share via the O(1) weak.Pointer[*bytecode.Proto] map when loaded as a Chunk).
	maxStructuralProtoCodeLen = 4096
)

type protoWeakKey struct {
	wp   weak.Pointer[bytecode.Proto]
	simd bool
}

type cachedTier1Entry struct {
	code         *jitCode
	hasFeedbackQ bool
}

// protoStructSig captures every immutable bytecode.Proto field read by Tier-1 baseline/SSA
// compilation so structural LRU matches are verified for exact equality without strongly
// retaining the original *bytecode.Proto or its child sub-proto tree.
type protoStructSig struct {
	simd       bool
	numParams  uint8
	maxStack   uint8
	isVararg   bool
	code       []bytecode.Instruction
	consts     []bytecode.Constant
	icSlot     []int32
	upvalNames []string
}

type structuralCacheNode struct {
	hash  [16]byte
	sig   protoStructSig
	entry cachedTier1Entry
	prev  *structuralCacheNode
	next  *structuralCacheNode
}

type jitCodeCache struct {
	mu      sync.Mutex
	byProto map[protoWeakKey]cachedTier1Entry
	byHash  map[[16]byte][]*structuralCacheNode
	lruHead *structuralCacheNode // most recently used
	lruTail *structuralCacheNode // least recently used
	lruLen  int
}

var globalJITCache = &jitCodeCache{
	byProto: make(map[protoWeakKey]cachedTier1Entry, 64),
	byHash:  make(map[[16]byte][]*structuralCacheNode, maxStructuralCacheEntries),
}

// getOrCompileSharedTier1 returns a State-owned jitFunc backed by a shared, reference-counted
// Tier-1 jitCode for bp.proto. Independent States loading the same *bytecode.Proto (or parsing
// identical Lua source via DoString) share a single sealed RX executable mapping.
func getOrCompileSharedTier1(bp *boundProto, enableSIMD bool) (*jitFunc, error) {
	p := bp.proto
	if p == nil || len(p.Code) == 0 {
		return compileBaselineNative(bp, &ir.Feedback{EnableSIMD: enableSIMD})
	}
	wk := protoWeakKey{wp: weak.Make(p), simd: enableSIMD}

	// Fast path 1: exact *bytecode.Proto pointer hit (O(1), zero hashing).
	globalJITCache.mu.Lock()
	if ent, ok := globalJITCache.byProto[wk]; ok {
		ent.code.retain()
		globalJITCache.mu.Unlock()
		return &jitFunc{
			proto:        p,
			code:         ent.code,
			pcEntries:    ent.code.pcEntries,
			tier:         1,
			hasFeedbackQ: ent.hasFeedbackQ,
		}, nil
	}

	// Fast path 2: structural fingerprint hit for identical DoString parses across States.
	canStruct := len(p.Code) <= maxStructuralProtoCodeLen
	var fp [16]byte
	if canStruct {
		fp = fingerprintProto(p, enableSIMD)
		if node := globalJITCache.findStructuralLocked(fp, p, enableSIMD); node != nil {
			globalJITCache.touchLocked(node)
			ent := node.entry
			globalJITCache.insertWeakLocked(p, wk, ent)
			ent.code.retain() // caller's ref
			globalJITCache.mu.Unlock()
			return &jitFunc{
				proto:        p,
				code:         ent.code,
				pcEntries:    ent.code.pcEntries,
				tier:         1,
				hasFeedbackQ: ent.hasFeedbackQ,
			}, nil
		}
	}
	globalJITCache.mu.Unlock()

	// Compile outside the mutex so concurrent VMs never serialize on codegen/execmem.
	compiled, err := compileBaselineNative(bp, &ir.Feedback{EnableSIMD: enableSIMD})
	if err != nil || compiled == nil || compiled.code == nil {
		return compiled, err
	}
	compiled.hasFeedbackQ = protoNeedsFeedback(p)
	newEnt := cachedTier1Entry{
		code:         compiled.code,
		hasFeedbackQ: compiled.hasFeedbackQ,
	}

	globalJITCache.mu.Lock()
	// Re-check in case another goroutine finished compiling the same proto concurrently.
	if ent, ok := globalJITCache.byProto[wk]; ok {
		ent.code.retain()
		globalJITCache.mu.Unlock()
		compiled.free() // release duplicate compiled region
		return &jitFunc{
			proto:        p,
			code:         ent.code,
			pcEntries:    ent.code.pcEntries,
			tier:         1,
			hasFeedbackQ: ent.hasFeedbackQ,
		}, nil
	}
	var evicted *jitCode
	if canStruct {
		if node := globalJITCache.findStructuralLocked(fp, p, enableSIMD); node != nil {
			globalJITCache.touchLocked(node)
			ent := node.entry
			globalJITCache.insertWeakLocked(p, wk, ent)
			ent.code.retain()
			globalJITCache.mu.Unlock()
			compiled.free() // release duplicate compiled region
			return &jitFunc{
				proto:        p,
				code:         ent.code,
				pcEntries:    ent.code.pcEntries,
				tier:         1,
				hasFeedbackQ: ent.hasFeedbackQ,
			}, nil
		}
		evicted = globalJITCache.insertStructuralLocked(fp, snapshotProtoSig(p, enableSIMD), newEnt)
	}
	globalJITCache.insertWeakLocked(p, wk, newEnt)
	globalJITCache.mu.Unlock()
	if evicted != nil {
		evicted.release()
	}

	return compiled, nil
}

func (c *jitCodeCache) insertWeakLocked(p *bytecode.Proto, wk protoWeakKey, ent cachedTier1Entry) {
	if _, exists := c.byProto[wk]; exists {
		return
	}
	ent.code.retain()
	c.byProto[wk] = ent
	runtime.AddCleanup(p, cleanupSharedProtoEntry, wk)
}

func cleanupSharedProtoEntry(wk protoWeakKey) {
	globalJITCache.mu.Lock()
	ent, ok := globalJITCache.byProto[wk]
	if ok {
		delete(globalJITCache.byProto, wk)
	}
	globalJITCache.mu.Unlock()
	if ok && ent.code != nil {
		ent.code.release()
	}
}

func (c *jitCodeCache) findStructuralLocked(fp [16]byte, p *bytecode.Proto, simd bool) *structuralCacheNode {
	bucket := c.byHash[fp]
	for _, node := range bucket {
		if sigMatchesProto(&node.sig, p, simd) {
			return node
		}
	}
	return nil
}

func (c *jitCodeCache) insertStructuralLocked(fp [16]byte, sig protoStructSig, ent cachedTier1Entry) *jitCode {
	ent.code.retain()
	node := &structuralCacheNode{
		hash:  fp,
		sig:   sig,
		entry: ent,
	}
	c.byHash[fp] = append(c.byHash[fp], node)
	c.pushFrontLocked(node)
	c.lruLen++
	if c.lruLen > maxStructuralCacheEntries {
		ev := c.lruTail
		c.removeNodeLocked(ev)
		c.lruLen--
		bucket := c.byHash[ev.hash]
		for i, n := range bucket {
			if n == ev {
				bucket[i] = bucket[len(bucket)-1]
				bucket = bucket[:len(bucket)-1]
				break
			}
		}
		if len(bucket) == 0 {
			delete(c.byHash, ev.hash)
		} else {
			c.byHash[ev.hash] = bucket
		}
		return ev.entry.code
	}
	return nil
}

func (c *jitCodeCache) touchLocked(node *structuralCacheNode) {
	if c.lruHead == node {
		return
	}
	c.removeNodeLocked(node)
	c.pushFrontLocked(node)
}

func (c *jitCodeCache) pushFrontLocked(node *structuralCacheNode) {
	node.prev = nil
	node.next = c.lruHead
	if c.lruHead != nil {
		c.lruHead.prev = node
	} else {
		c.lruTail = node
	}
	c.lruHead = node
}

func (c *jitCodeCache) removeNodeLocked(node *structuralCacheNode) {
	if node.prev != nil {
		node.prev.next = node.next
	} else {
		c.lruHead = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	} else {
		c.lruTail = node.prev
	}
	node.prev = nil
	node.next = nil
}

// clearSharedJITCodeCache releases all cached references held by the global cache (for tests).
// Live States retain their own references and continue executing safely until closed.
func clearSharedJITCodeCache() {
	globalJITCache.mu.Lock()
	var toRelease []*jitCode
	for k, ent := range globalJITCache.byProto {
		delete(globalJITCache.byProto, k)
		if ent.code != nil {
			toRelease = append(toRelease, ent.code)
		}
	}
	for n := globalJITCache.lruHead; n != nil; n = n.next {
		if n.entry.code != nil {
			toRelease = append(toRelease, n.entry.code)
		}
	}
	clear(globalJITCache.byHash)
	globalJITCache.lruHead = nil
	globalJITCache.lruTail = nil
	globalJITCache.lruLen = 0
	globalJITCache.mu.Unlock()
	for _, code := range toRelease {
		code.release()
	}
}

func fingerprintProto(p *bytecode.Proto, simd bool) [16]byte {
	h := sha256.New()
	var buf [8]byte
	putU8 := func(v uint8) {
		buf[0] = v
		h.Write(buf[:1])
	}
	putU32 := func(v uint32) {
		binary.LittleEndian.PutUint32(buf[:4], v)
		h.Write(buf[:4])
	}
	putU64 := func(v uint64) {
		binary.LittleEndian.PutUint64(buf[:8], v)
		h.Write(buf[:8])
	}

	if simd {
		putU8(1)
	} else {
		putU8(0)
	}
	putU8(p.NumParams)
	putU8(p.MaxStackSize)
	if p.IsVararg() {
		putU8(1)
	} else {
		putU8(0)
	}
	putU32(uint32(len(p.Code)))
	for _, w := range p.Code {
		putU32(uint32(w))
	}
	putU32(uint32(len(p.Consts)))
	for _, k := range p.Consts {
		putU8(uint8(k.Kind))
		switch k.Kind {
		case bytecode.ConstInt:
			putU64(uint64(k.Int))
		case bytecode.ConstFloat:
			putU64(math.Float64bits(k.Float))
		case bytecode.ConstString:
			putU32(uint32(len(k.Str)))
			h.Write([]byte(k.Str))
		}
	}
	putU32(uint32(len(p.ICSlot)))
	for _, s := range p.ICSlot {
		putU32(uint32(s))
	}
	putU32(uint32(len(p.Upvalues)))
	for _, uv := range p.Upvalues {
		putU32(uint32(len(uv.Name)))
		h.Write([]byte(uv.Name))
	}
	sum := h.Sum(nil)
	var out [16]byte
	copy(out[:], sum[:16])
	return out
}

func snapshotProtoSig(p *bytecode.Proto, simd bool) protoStructSig {
	upNames := make([]string, len(p.Upvalues))
	for i, uv := range p.Upvalues {
		upNames[i] = uv.Name
	}
	return protoStructSig{
		simd:       simd,
		numParams:  p.NumParams,
		maxStack:   p.MaxStackSize,
		isVararg:   p.IsVararg(),
		code:       slices.Clone(p.Code),
		consts:     slices.Clone(p.Consts),
		icSlot:     slices.Clone(p.ICSlot),
		upvalNames: upNames,
	}
}

func sigMatchesProto(sig *protoStructSig, p *bytecode.Proto, simd bool) bool {
	if sig.simd != simd ||
		sig.numParams != p.NumParams ||
		sig.maxStack != p.MaxStackSize ||
		sig.isVararg != p.IsVararg() ||
		len(sig.code) != len(p.Code) ||
		len(sig.consts) != len(p.Consts) ||
		len(sig.icSlot) != len(p.ICSlot) ||
		len(sig.upvalNames) != len(p.Upvalues) {
		return false
	}
	if !slices.Equal(sig.code, p.Code) || !slices.Equal(sig.icSlot, p.ICSlot) {
		return false
	}
	for i, k := range p.Consts {
		sk := sig.consts[i]
		if sk.Kind != k.Kind {
			return false
		}
		switch k.Kind {
		case bytecode.ConstInt:
			if sk.Int != k.Int {
				return false
			}
		case bytecode.ConstFloat:
			if math.Float64bits(sk.Float) != math.Float64bits(k.Float) {
				return false
			}
		case bytecode.ConstString:
			if sk.Str != k.Str {
				return false
			}
		}
	}
	for i, uv := range p.Upvalues {
		if sig.upvalNames[i] != uv.Name {
			return false
		}
	}
	return true
}
