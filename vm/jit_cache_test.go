//go:build (arm64 || amd64) && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"fmt"
	"sync"
	"testing"

	"github.com/yuchanns/moelua/compiler"
)

func TestJITSharedCodeCacheAcrossConcurrentStates(t *testing.T) {
	clearSharedJITCodeCache()
	t.Cleanup(clearSharedJITCodeCache)

	const src = `
local sum = 0
for i = 1, 500 do
  sum = sum + i * 3
end
return sum
`
	p, err := compiler.Compile("=shared_chunk", src)
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 16
	const statesPerG = 4
	addrs := make([]uintptr, goroutines*statesPerG)
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*statesPerG)

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for k := range statesPerG {
				s := NewState()
				s.SetJITEnabled(true)
				cl, err := s.Load(p)
				if err != nil {
					s.Close()
					errs <- err
					return
				}
				rets, err := s.Call(ClosureValue(cl))
				if err != nil {
					s.Close()
					errs <- err
					return
				}
				if len(rets) != 1 || rets[0].Int() != 375750 {
					s.Close()
					errs <- fmt.Errorf("unexpected result: rets=%v", rets)
					return
				}
				bp := s.boundProtoOf(cl)
				jf := s.jit.funcs[bp.protoIdx]
				if jf == nil || jf.code == nil || jf.code.region == nil {
					s.Close()
					t.Errorf("expected compiled jitFunc for shared proto")
					return
				}
				addrs[g*statesPerG+k] = jf.code.region.Addr()
				s.Close()
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	first := addrs[0]
	if first == 0 {
		t.Fatal("expected non-zero RX region address")
	}
	for i, a := range addrs {
		if a != first {
			t.Fatalf("state %d got distinct RX region %#x, want shared %#x", i, a, first)
		}
	}
}

func TestJITSharedCodeCacheStructuralDedupAndRefcountLifetime(t *testing.T) {
	clearSharedJITCodeCache()
	t.Cleanup(clearSharedJITCodeCache)

	const src = `
local function makeTree(d)
  if d == 0 then return { x = 1, y = 2 } end
  return { l = makeTree(d - 1), r = makeTree(d - 1), tag = d }
end
local t = makeTree(6)
return t.tag
`
	const n = 32
	states := make([]*State, n)
	closures := make([]*Closure, n)
	var sharedCode *jitCode
	var sharedAddr uintptr

	for i := range n {
		p, err := compiler.Compile("=dup_dostring", src)
		if err != nil {
			t.Fatal(err)
		}
		s := NewState()
		s.SetJITEnabled(true)
		cl, err := s.Load(p)
		if err != nil {
			s.Close()
			t.Fatal(err)
		}
		rets, err := s.Call(ClosureValue(cl))
		if err != nil || len(rets) != 1 || rets[0].Int() != 6 {
			s.Close()
			t.Fatalf("state %d run failed: rets=%v err=%v", i, rets, err)
		}
		// Find the compiled makeTree sub-proto in s.jit.funcs
		var found *jitFunc
		for _, jf := range s.jit.funcs {
			if jf != nil && jf.proto != nil && jf.proto.LineDefined == 2 {
				found = jf
				break
			}
		}
		if found == nil || found.code == nil || found.code.region == nil {
			s.Close()
			t.Fatalf("state %d did not compile makeTree", i)
		}
		if i == 0 {
			sharedCode = found.code
			sharedAddr = found.code.region.Addr()
		} else if found.code.region.Addr() != sharedAddr {
			s.Close()
			t.Fatalf("state %d allocated separate RX region %#x, want shared %#x", i, found.code.region.Addr(), sharedAddr)
		}
		states[i] = s
		closures[i] = cl
	}

	// Clear global cache and close half the States while the rest continue executing safely!
	clearSharedJITCodeCache()
	for i := 0; i < n/2; i++ {
		states[i].Close()
		states[i] = nil
	}
	if sharedCode.refs.Load() != int32(n-n/2) {
		t.Fatalf("expected %d live State refs after cache clear & half close, got %d", n-n/2, sharedCode.refs.Load())
	}
	// Re-execute the original closure on a surviving State to prove sharedCode RX memory is still mapped & valid!
	rets, err := states[n-1].Call(ClosureValue(closures[n-1]))
	if err != nil || len(rets) != 1 || rets[0].Int() != 6 {
		t.Fatalf("surviving state execution failed: rets=%v err=%v", rets, err)
	}
	// Close all remaining States -> refcount reaches 0 and region is freed!
	for i := n / 2; i < n; i++ {
		states[i].Close()
	}
	if sharedCode.refs.Load() != 0 || sharedCode.region != nil {
		t.Fatalf("expected shared jitCode to be freed (refs=%d, region=%v)", sharedCode.refs.Load(), sharedCode.region)
	}
}

func TestJITSharedCodeCacheSeparatesSIMDToggle(t *testing.T) {
	clearSharedJITCodeCache()
	t.Cleanup(clearSharedJITCodeCache)

	const src = `
local a, b = {}, {}
for i = 1, 32 do a[i] = i * 1.0; b[i] = 2.0 end
for i = 1, 32 do a[i] = a[i] + 1.5 * b[i] end
return a[32]
`
	p, err := compiler.Compile("=simd_key", src)
	if err != nil {
		t.Fatal(err)
	}

	s1 := NewState()
	defer s1.Close()
	s1.SetJITEnabled(true)
	s1.SetJITSIMDEnabled(true)
	cl1, _ := s1.Load(p)
	r1, err := s1.Call(ClosureValue(cl1))
	if err != nil || r1[0].Float() != 35.0 {
		t.Fatalf("s1 failed: %v %v", r1, err)
	}

	s2 := NewState()
	defer s2.Close()
	s2.SetJITEnabled(true)
	s2.SetJITSIMDEnabled(false)
	cl2, _ := s2.Load(p)
	r2, err := s2.Call(ClosureValue(cl2))
	if err != nil || r2[0].Float() != 35.0 {
		t.Fatalf("s2 failed: %v %v", r2, err)
	}

	jf1 := s1.jit.funcs[s1.boundProtoOf(cl1).protoIdx]
	jf2 := s2.jit.funcs[s2.boundProtoOf(cl2).protoIdx]
	if jf1.code.region.Addr() == jf2.code.region.Addr() {
		t.Fatalf("expected distinct cache entries for SIMD enabled vs disabled, both got %#x", jf1.code.region.Addr())
	}
}
