package vm

import (
	"runtime"
	"sync"

	"github.com/yuchanns/moelua/internal/jit/execmem"
)

var (
	jitProbeOnce sync.Once
	jitProbeOK   bool
)

// jitRuntimeAvailable probes OS W^X executable page creation once on first opt-in so
// restricted environments (e.g. hardened containers or SELinux execmem denial) fall back
// cleanly to the Plan 9 / Go interpreter instead of failing mid-execution.
func jitRuntimeAvailable() bool {
	if !execmem.Supported() {
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
	return execmem.Supported()
}

// jitState holds optional per-State JIT code regions and compilation state.
// It stays nil unless a caller opts in via SetJITEnabled(true) / moelua.WithJIT(),
// keeping idle and noJIT States at zero extra memory and zero dispatch overhead.
type jitState struct {
	enabled bool
	regions []*execmem.Region
}

func (j *jitState) close() {
	if j == nil {
		return
	}
	for _, r := range j.regions {
		r.Free()
	}
	j.regions = nil
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

func (s *State) closeJIT() {
	if s.jit != nil {
		s.jit.close()
		s.jit = nil
	}
}
