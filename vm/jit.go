package vm

import (
	"runtime"
	"sync"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/jit/execmem"
)

const (
	// jitEntryCold marks a protoIdx that has not yet been JIT-compiled.
	jitEntryCold uintptr = 0
	// jitEntrySkip marks a protoIdx that could not be compiled and should stay interpreted.
	jitEntrySkip uintptr = 1
	// jitCompileThreshold is the call count before compiling loop-free functions.
	// Functions containing backward branches or numeric/generic loops compile on first entry.
	jitCompileThreshold = 4
)

var (
	jitProbeOnce sync.Once
	jitProbeOK   bool
)

// jitRuntimeAvailable probes OS W^X executable page creation once on first opt-in so
// restricted environments (e.g. hardened containers or SELinux execmem denial) fall back
// cleanly to the Plan 9 / Go interpreter instead of failing mid-execution.
func jitRuntimeAvailable() bool {
	if !jitLayoutSupported || !execmem.Supported() {
		return false
	}
	jitProbeOnce.Do(func() {
		var retStub []byte
		switch runtime.GOARCH {
		case "arm64":
			retStub = []byte{0xc0, 0x03, 0x5f, 0xd6} // RET
		case "amd64":
			retStub = []byte{0xc3} // RET
		default:
			return
		}
		reg, err := execmem.Publish(retStub)
		if err != nil {
			return
		}
		reg.Free()
		jitProbeOK = true
	})
	return jitProbeOK
}

// JITSupported reports whether the native JIT backend is supported in this build.
// Note that OS security policies may still deny PROT_EXEC at runtime, in which case
// SetJITEnabled(true) gracefully falls back to the interpreter.
func JITSupported() bool {
	return jitLayoutSupported && execmem.Supported()
}

// jitFunc owns the sealed executable code region and per-bytecode-PC native entry table
// for one compiled *bytecode.Proto.
type jitFunc struct {
	proto        *bytecode.Proto
	region       *execmem.Region
	pcEntries    []uintptr // len == len(proto.Code); pcEntries[pc] is the native resume address
	tier         uint8     // 1 = baseline + static SSA loops; 2 = recompiled with live IC/callee feedback
	hasFeedbackQ bool      // true when proto contains loops with GETFIELD/SETFIELD/CALL that benefit from Tier-2 feedback
}

func (f *jitFunc) free() {
	if f == nil {
		return
	}
	if f.region != nil {
		f.region.Free()
		f.region = nil
	}
	f.pcEntries = nil
	f.proto = nil
}

// jitState holds optional per-State JIT code regions and compilation tables.
// It stays nil unless a caller opts in via SetJITEnabled(true) / moelua.WithJIT(),
// keeping idle and noJIT States at zero extra memory and zero dispatch overhead.
type jitState struct {
	enabled     bool
	disableSIMD bool       // optional toggle for scalar-vs-SIMD A/B benchmarking
	entries     []uintptr  // indexed by protoIdx: 0=cold, 1=skip, >=4096=pcEntries[0]
	pcTables    []uintptr  // indexed by protoIdx: &funcs[i].pcEntries[0] (0 if uncompiled)
	hotness     []uint16   // indexed by protoIdx: call count for loop-free functions & Tier-2 feedback trigger
	funcs       []*jitFunc // indexed by protoIdx
}

func (j *jitState) ensureIdx(idx uint32) {
	need := int(idx) + 1
	if need <= len(j.entries) {
		return
	}
	newCap := max(need, len(j.entries)*2)
	if newCap < 16 {
		newCap = 16
	}
	growU := func(s []uintptr) []uintptr {
		n := make([]uintptr, newCap)
		copy(n, s)
		return n
	}
	j.entries = growU(j.entries)
	j.pcTables = growU(j.pcTables)
	nh := make([]uint16, newCap)
	copy(nh, j.hotness)
	j.hotness = nh
	nf := make([]*jitFunc, newCap)
	copy(nf, j.funcs)
	j.funcs = nf
}

func (j *jitState) invalidateProto(idx uint32) {
	if j == nil || int(idx) >= len(j.entries) {
		return
	}
	if f := j.funcs[idx]; f != nil {
		f.free()
		j.funcs[idx] = nil
	}
	j.entries[idx] = jitEntryCold
	j.pcTables[idx] = 0
	j.hotness[idx] = 0
}

func (j *jitState) close() {
	if j == nil {
		return
	}
	for i, f := range j.funcs {
		if f != nil {
			f.free()
			j.funcs[i] = nil
		}
	}
	j.entries = nil
	j.pcTables = nil
	j.hotness = nil
	j.funcs = nil
	j.enabled = false
}

// SetJITEnabled enables or disables the optional native JIT tier for this State.
// On unsupported platforms, -tags moeluanojit builds, or OS environments denying W^X
// executable memory, enabling JIT is a safe no-op that falls back to the interpreter.
func (s *State) SetJITEnabled(enabled bool) {
	if !enabled {
		if s.jit != nil {
			s.jit.enabled = false
		}
		return
	}
	if !jitRuntimeAvailable() {
		return
	}
	if s.jit == nil {
		s.jit = &jitState{}
	}
	s.jit.enabled = true
}

// JITEnabled reports whether the native JIT tier is currently active on this State.
func (s *State) JITEnabled() bool {
	return s.jit != nil && s.jit.enabled
}

// SetJITSIMDEnabled controls whether the optimizing JIT auto-vectorizes eligible stride-1
// loops using ARM64 NEON / AMD64 SSE2 (enabled by default when JIT is enabled).
func (s *State) SetJITSIMDEnabled(enabled bool) {
	if s.jit == nil {
		if !enabled {
			s.jit = &jitState{disableSIMD: true}
		}
		return
	}
	s.jit.disableSIMD = !enabled
}

func (s *State) closeJIT() {
	if s.jit != nil {
		s.jit.close()
		s.jit = nil
	}
}
